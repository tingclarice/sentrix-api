//go:build integration

package store

import (
	"context"
	"errors"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Run against a dev database with: go test -tags integration ./internal/store
// It borrows an existing user, creates a throwaway group and device, and deletes
// them afterwards. Never point it at production.
func TestPGAgainstRealDatabase(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Cleanups run last-in first-out, so this closes the pool after the cleanup below.
	t.Cleanup(db.Close)

	var userID string
	if err := db.QueryRow(ctx, `SELECT id FROM users LIMIT 1`).Scan(&userID); err != nil {
		t.Skipf("no user to borrow: %v", err)
	}

	const mac, pin = "0DEADBEEF001", "654321"
	db.Exec(ctx, `DELETE FROM devices WHERE mac_address = $1`, mac)
	if _, err := db.Exec(ctx, `INSERT INTO devices (mac_address, pin_code) VALUES ($1, $2)`, mac, pin); err != nil {
		t.Fatal(err)
	}
	var groupID string
	t.Cleanup(func() {
		db.Exec(ctx, `DELETE FROM devices WHERE mac_address = $1`, mac)
		if groupID != "" {
			db.Exec(ctx, `DELETE FROM groups WHERE id = $1`, groupID)
		}
	})

	s := NewPG(db)

	desc := "integration test"
	g, err := s.CreateGroup(ctx, userID, "store test", &desc)
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	groupID = g.ID
	if !regexp.MustCompile(`^[0-9A-F]{12}$`).MatchString(g.MeshID) {
		t.Errorf("mesh id %q is not 12 uppercase hex characters", g.MeshID)
	}

	groups, err := s.ListGroups(ctx, userID)
	if err != nil || len(groups) == 0 {
		t.Fatalf("ListGroups = %v, %v", groups, err)
	}

	if err := s.GroupOwnedBy(ctx, g.ID, userID); err != nil {
		t.Errorf("owner: %v", err)
	}
	if err := s.GroupOwnedBy(ctx, g.ID, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Errorf("another user: %v, want ErrNotFound", err)
	}

	// Wrong PIN, wrong MAC: both ErrNotFound and nothing changes.
	if _, err := s.ClaimDevice(ctx, g.ID, mac, "000000", nil, 5); !errors.Is(err, ErrNotFound) {
		t.Errorf("wrong pin: %v, want ErrNotFound", err)
	}
	if _, err := s.ClaimDevice(ctx, g.ID, "0DEADBEEF999", pin, nil, 5); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown mac: %v, want ErrNotFound", err)
	}

	name := "Pump 1"
	d, err := s.ClaimDevice(ctx, g.ID, mac, pin, &name, 5)
	if err != nil {
		t.Fatalf("ClaimDevice: %v", err)
	}
	if d.Status != "CALIBRATING" || d.GroupID == nil || *d.GroupID != g.ID || d.Name == nil || *d.Name != "Pump 1" {
		t.Errorf("claimed device = %+v", d)
	}
	if d.CalibrationStartedAt == nil || d.CalibrationEndsAt == nil {
		t.Fatalf("calibration times not set: %+v", d)
	}
	if got := d.CalibrationEndsAt.Sub(*d.CalibrationStartedAt).Minutes(); got < 4.99 || got > 5.01 {
		t.Errorf("calibration lasts %.2f minutes, want 5", got)
	}

	// A second claim fails: the device is no longer UNCLAIMED.
	if _, err := s.ClaimDevice(ctx, g.ID, mac, pin, nil, 5); !errors.Is(err, ErrNotFound) {
		t.Errorf("second claim: %v, want ErrNotFound", err)
	}

	devices, err := s.ListDevices(ctx, g.ID)
	if err != nil || len(devices) != 1 || devices[0].MACAddress != mac {
		t.Errorf("ListDevices = %+v, %v", devices, err)
	}

	// Read endpoints.
	if got, err := s.GetGroup(ctx, g.ID, userID); err != nil || got.ID != g.ID {
		t.Errorf("GetGroup = %+v, %v", got, err)
	}
	if _, err := s.GetGroup(ctx, g.ID, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetGroup for another user: %v, want ErrNotFound", err)
	}
	if err := s.DeviceOwnedBy(ctx, d.ID, userID); err != nil {
		t.Errorf("DeviceOwnedBy owner: %v", err)
	}
	if err := s.DeviceOwnedBy(ctx, d.ID, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeviceOwnedBy another user: %v, want ErrNotFound", err)
	}

	// 120 readings, one a minute, with one spike; ends a minute ago.
	base := time.Now().Truncate(time.Minute).Add(-120 * time.Minute)
	for i := 0; i < 120; i++ {
		amps := 4.0
		if i == 100 {
			amps = 15
		}
		if _, err := db.Exec(ctx, `INSERT INTO telemetry (device_id, "timestamp", rms_current) VALUES ($1, $2, $3)`,
			d.ID, base.Add(time.Duration(i)*time.Minute), amps); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := s.Telemetry(ctx, d.ID, time.Now().Add(-time.Hour), 0)
	if err != nil || len(raw) < 55 || len(raw) > 61 {
		t.Errorf("raw last hour: %d points, %v, want about 60", len(raw), err)
	}
	for i := 1; i < len(raw); i++ {
		if raw[i].Timestamp.Before(raw[i-1].Timestamp) {
			t.Fatal("raw points are not in time order")
		}
	}
	bucketed, err := s.Telemetry(ctx, d.ID, base.Add(-time.Minute), 15*time.Minute)
	if err != nil || len(bucketed) < 8 || len(bucketed) > 10 {
		t.Fatalf("15 minute buckets over 2h: %d points, %v, want 8-9", len(bucketed), err)
	}
	peak := 0.0
	for _, p := range bucketed {
		if p.MaxRMSCurrent > peak {
			peak = p.MaxRMSCurrent
		}
		if p.RMSCurrent > p.MaxRMSCurrent {
			t.Errorf("bucket average %.2f above its max %.2f", p.RMSCurrent, p.MaxRMSCurrent)
		}
	}
	if peak != 15 {
		t.Errorf("bucketed max = %.1f, want the 15A spike to survive averaging", peak)
	}

	// Bounds: one past hour, one future hour.
	hour := time.Now().Truncate(time.Hour)
	for _, h := range []time.Duration{-3 * time.Hour, 5 * time.Hour} {
		if _, err := db.Exec(ctx, `INSERT INTO prediction_bounds (device_id, "timestamp", upper_bound, lower_bound) VALUES ($1, $2, 7, 3)`,
			d.ID, hour.Add(h)); err != nil {
			t.Fatal(err)
		}
	}
	bounds, err := s.Bounds(ctx, d.ID, hour.Add(-time.Hour))
	if err != nil || len(bounds) != 1 || !bounds[0].Timestamp.Equal(hour.Add(5*time.Hour)) || bounds[0].UpperBound != 7 {
		t.Errorf("Bounds from 1h ago = %+v, %v, want only the future hour", bounds, err)
	}
	if all, _ := s.Bounds(ctx, d.ID, hour.Add(-4*time.Hour)); len(all) != 2 {
		t.Errorf("Bounds from 4h ago = %d rows, want 2", len(all))
	}

	// Summary counts the new CALIBRATING device and an unresolved alert from now.
	before, err := s.Summary(ctx, userID)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if before.Devices < 1 || before.Calibrating < 1 || before.Groups < 1 {
		t.Errorf("Summary = %+v, want at least this group and device", before)
	}
	if _, err := db.Exec(ctx, `INSERT INTO alerts (device_id, "timestamp", actual_value, expected_max, expected_min)
		VALUES ($1, now(), 15, 7, 3)`, d.ID); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Summary(ctx, userID)
	if after.Anomalies24h != before.Anomalies24h+1 || after.UnresolvedAlerts != before.UnresolvedAlerts+1 {
		t.Errorf("Summary alerts before %+v after %+v, want +1 each", before, after)
	}

	// Alert feed. Add an older second alert so ordering and paging can be checked.
	if _, err := db.Exec(ctx, `INSERT INTO alerts (device_id, "timestamp", actual_value, expected_max, expected_min, severity)
		VALUES ($1, now() - interval '2 hours', 1, 7, 3, 'WARNING')`, d.ID); err != nil {
		t.Fatal(err)
	}
	mine := func(f AlertFilter) []Alert {
		t.Helper()
		f.DeviceID = d.ID // only this test's device, whatever else the user has
		if f.Limit == 0 {
			f.Limit = 50
		}
		alerts, err := s.ListAlerts(ctx, userID, f)
		if err != nil {
			t.Fatalf("ListAlerts(%+v): %v", f, err)
		}
		return alerts
	}
	feed := mine(AlertFilter{})
	if len(feed) != 2 || feed[0].ActualValue != 15 || feed[1].ActualValue != 1 {
		t.Fatalf("feed = %+v, want the 15A alert then the older 1A one", feed)
	}
	top := feed[0]
	if top.DeviceMAC != mac || top.GroupID != g.ID || top.DeviceName == nil || *top.DeviceName != "Pump 1" ||
		top.Severity != "WARNING" || top.IsResolved || top.UserFeedback != nil {
		t.Errorf("alert = %+v, want device details, default severity, unresolved, no feedback", top)
	}
	if got := mine(AlertFilter{Limit: 1}); len(got) != 1 || got[0].ID != top.ID {
		t.Errorf("limit 1 = %+v", got)
	}
	if got := mine(AlertFilter{Before: &top.Timestamp}); len(got) != 1 || got[0].ActualValue != 1 {
		t.Errorf("before the newest = %+v, want only the older alert", got)
	}

	reviewedFalse := false
	if got := mine(AlertFilter{Reviewed: &reviewedFalse}); len(got) != 2 {
		t.Errorf("unreviewed = %d, want 2", len(got))
	}
	fb, err := s.SetAlertFeedback(ctx, top.ID, userID, false) // false alarm
	if err != nil || fb.UserFeedback == nil || *fb.UserFeedback != false {
		t.Fatalf("SetAlertFeedback = %+v, %v", fb, err)
	}
	reviewedTrue := true
	if got := mine(AlertFilter{Reviewed: &reviewedTrue}); len(got) != 1 || got[0].ID != top.ID {
		t.Errorf("reviewed = %+v, want just the alert with feedback", got)
	}
	if got := mine(AlertFilter{Reviewed: &reviewedFalse}); len(got) != 1 {
		t.Errorf("unreviewed after feedback = %d, want 1", len(got))
	}

	res, err := s.ResolveAlert(ctx, top.ID, userID)
	if err != nil || !res.IsResolved {
		t.Fatalf("ResolveAlert = %+v, %v", res, err)
	}
	if got := mine(AlertFilter{}); len(got) != 1 { // default hides resolved
		t.Errorf("unresolved after resolving = %d, want 1", len(got))
	}
	if got := mine(AlertFilter{Status: "resolved"}); len(got) != 1 || got[0].ID != top.ID {
		t.Errorf("resolved = %+v", got)
	}
	if got := mine(AlertFilter{Status: "all"}); len(got) != 2 {
		t.Errorf("all = %d, want 2", len(got))
	}
	if got := mine(AlertFilter{GroupID: g.ID, Status: "all"}); len(got) != 2 {
		t.Errorf("by group = %d, want 2", len(got))
	}

	// Another user can neither see nor change these alerts.
	stranger := "00000000-0000-0000-0000-000000000000"
	if got, err := s.ListAlerts(ctx, stranger, AlertFilter{Status: "all", Limit: 50}); err != nil || len(got) != 0 {
		t.Errorf("stranger's feed = %+v, %v, want empty", got, err)
	}
	if _, err := s.SetAlertFeedback(ctx, top.ID, stranger, true); !errors.Is(err, ErrNotFound) {
		t.Errorf("stranger feedback: %v, want ErrNotFound", err)
	}
	if _, err := s.ResolveAlert(ctx, top.ID, stranger); !errors.Is(err, ErrNotFound) {
		t.Errorf("stranger resolve: %v, want ErrNotFound", err)
	}
}
