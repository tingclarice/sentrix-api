package ingest

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// Alert rules (backend spec section 6, architecture gotcha #7).
const (
	// standbyAmps: readings below this are treated as the machine being off or idle
	// and are neither checked nor counted as a breach.
	standbyAmps = 0.1
	// breachThreshold consecutive out-of-bounds readings are needed before alerting.
	// Bounds are hourly, so a single minute outside them is mostly noise.
	breachThreshold = 3
	// alertCooldown stops a device that recovers and breaches again right away from
	// raising a new alert every few minutes.
	alertCooldown = 30 * time.Minute
	// maxReadingAge: readings older than this are stored but never alerted on, so a
	// backfill or a redelivered message cannot trigger an alert storm.
	maxReadingAge = 10 * time.Minute

	boundTTL     = 10 * time.Minute // a bound found for an hour is reused this long
	noBoundRetry = time.Minute      // an hour with no bound is looked up again this often
)

type bound struct {
	Lower, Upper float64
}

// severity is CRITICAL when the reading is more than one full band width outside
// the bounds, otherwise WARNING.
func severity(value float64, b bound) string {
	width := b.Upper - b.Lower
	if width < 0.1 {
		width = 0.1 // a very narrow band should not make every blip critical
	}
	var deviation float64
	switch {
	case value > b.Upper:
		deviation = value - b.Upper
	case value < b.Lower:
		deviation = b.Lower - value
	}
	if deviation > width {
		return "CRITICAL"
	}
	return "WARNING"
}

func breached(value float64, b bound) bool { return value > b.Upper || value < b.Lower }

type breachState struct {
	streak    int
	alerted   bool
	lastAlert time.Time
}

// breachTracker counts consecutive breaches per device. It is in memory, so the
// count restarts from zero when the service restarts.
type breachTracker struct {
	mu     sync.Mutex
	states map[string]*breachState
}

func newBreachTracker() *breachTracker {
	return &breachTracker{states: make(map[string]*breachState)}
}

// reset forgets a device's streak: it was in bounds, idle, or has no bound to check.
func (t *breachTracker) reset(deviceID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if st, ok := t.states[deviceID]; ok {
		st.streak, st.alerted = 0, false
	}
}

// breach records an out-of-bounds reading taken at ts and reports whether an alert
// should be raised: once per run of breaches, after breachThreshold in a row, and
// not within alertCooldown of the previous alert. Time comes from the reading, not
// the wall clock, so replayed data behaves the same as live data.
func (t *breachTracker) breach(deviceID string, ts time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.states[deviceID]
	if st == nil {
		st = &breachState{}
		t.states[deviceID] = st
	}
	st.streak++
	if st.streak < breachThreshold || st.alerted {
		return false
	}
	if !st.lastAlert.IsZero() && ts.Sub(st.lastAlert) < alertCooldown {
		return false
	}
	st.alerted, st.lastAlert = true, ts
	return true
}

type boundKey struct {
	deviceID string
	hour     time.Time
}

type boundEntry struct {
	b       bound
	found   bool
	fetched time.Time
}

// boundCache avoids a query per reading: bounds are one row per hour, so each
// device-hour is fetched once.
type boundCache struct {
	mu  sync.Mutex
	now func() time.Time
	m   map[boundKey]boundEntry
}

func newBoundCache() *boundCache {
	return &boundCache{now: time.Now, m: make(map[boundKey]boundEntry)}
}

// get returns the bound for the device-hour, calling load on a miss or when the
// entry is stale. found is false when no bound exists for that hour.
func (c *boundCache) get(ctx context.Context, deviceID string, hour time.Time,
	load func(context.Context, string, time.Time) (bound, bool, error)) (bound, bool, error) {
	key := boundKey{deviceID, hour}
	c.mu.Lock()
	e, ok := c.m[key]
	c.mu.Unlock()
	if ok {
		ttl := boundTTL
		if !e.found {
			ttl = noBoundRetry
		}
		if c.now().Sub(e.fetched) < ttl {
			return e.b, e.found, nil
		}
	}

	b, found, err := load(ctx, deviceID, hour)
	if err != nil {
		return bound{}, false, err
	}
	c.mu.Lock()
	// Drop entries from earlier hours so the map does not grow forever.
	for k := range c.m {
		if k.hour.Before(hour.Add(-2 * time.Hour)) {
			delete(c.m, k)
		}
	}
	c.m[key] = boundEntry{b: b, found: found, fetched: c.now()}
	c.mu.Unlock()
	return b, found, nil
}

func (s *Service) loadBound(ctx context.Context, deviceID string, hour time.Time) (bound, bool, error) {
	var b bound
	err := s.db.QueryRow(ctx,
		`SELECT lower_bound, upper_bound FROM prediction_bounds
		 WHERE device_id = $1 AND "timestamp" = $2`, deviceID, hour).Scan(&b.Lower, &b.Upper)
	if errors.Is(err, pgx.ErrNoRows) {
		return bound{}, false, nil
	}
	return b, err == nil, err
}

// checkAlert compares one reading from an ACTIVE device with its hourly bound and
// inserts an alert when the rules in this file say so.
func (s *Service) checkAlert(ctx context.Context, deviceID string, r Reading) error {
	ts := time.Unix(r.TS, 0)
	if time.Since(ts) > maxReadingAge {
		return nil // a replayed or redelivered reading, not a live anomaly
	}
	if r.RMSCurrent < standbyAmps {
		s.breaches.reset(deviceID)
		return nil
	}
	b, found, err := s.bounds.get(ctx, deviceID, ts.Truncate(time.Hour), s.loadBound)
	if err != nil {
		return fmt.Errorf("load bound: %w", err)
	}
	if !found || !breached(r.RMSCurrent, b) {
		s.breaches.reset(deviceID)
		return nil
	}
	if !s.breaches.breach(deviceID, ts) {
		return nil
	}

	sev := severity(r.RMSCurrent, b)
	_, err = s.db.Exec(ctx, `
		INSERT INTO alerts (device_id, "timestamp", actual_value, expected_max, expected_min, severity)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		deviceID, ts, r.RMSCurrent, b.Upper, b.Lower, sev)
	if err != nil {
		return fmt.Errorf("insert alert: %w", err)
	}
	log.Printf("alert: device %s %s, %.2fA outside %.2f-%.2fA", deviceID, sev, r.RMSCurrent, b.Lower, b.Upper)
	return nil
}
