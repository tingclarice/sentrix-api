package ingest

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestBreached(t *testing.T) {
	b := bound{Lower: 4, Upper: 7}
	for _, tt := range []struct {
		v    float64
		want bool
	}{{5, false}, {4, false}, {7, false}, {7.01, true}, {3.99, true}, {15, true}} {
		if got := breached(tt.v, b); got != tt.want {
			t.Errorf("breached(%v) = %v, want %v", tt.v, got, tt.want)
		}
	}
}

func TestSeverity(t *testing.T) {
	b := bound{Lower: 4, Upper: 7} // width 3
	for _, tt := range []struct {
		v    float64
		want string
	}{
		{7.5, "WARNING"},    // 0.5 above
		{10, "WARNING"},     // exactly one band width above is still a warning
		{10.01, "CRITICAL"}, // more than a band width above
		{15, "CRITICAL"},
		{3.5, "WARNING"},
		{0.5, "CRITICAL"}, // 3.5 below
	} {
		if got := severity(tt.v, b); got != tt.want {
			t.Errorf("severity(%v) = %s, want %s", tt.v, got, tt.want)
		}
	}
	// A very narrow band must not turn every blip critical.
	if got := severity(5.12, bound{Lower: 5, Upper: 5.05}); got != "WARNING" {
		t.Errorf("narrow band: %s, want WARNING", got)
	}
}

func TestBreachTracker(t *testing.T) {
	t0 := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	at := func(min int) time.Time { return t0.Add(time.Duration(min) * time.Minute) }

	t.Run("fires on the third breach in a row, once", func(t *testing.T) {
		tr := newBreachTracker()
		var fired []int
		for i := 0; i < 10; i++ {
			if tr.breach("d", at(i)) {
				fired = append(fired, i)
			}
		}
		if len(fired) != 1 || fired[0] != breachThreshold-1 {
			t.Errorf("fired at %v, want once at index %d", fired, breachThreshold-1)
		}
	})

	t.Run("a good reading resets the streak", func(t *testing.T) {
		tr := newBreachTracker()
		tr.breach("d", at(0))
		tr.breach("d", at(1))
		tr.reset("d")
		if tr.breach("d", at(2)) || tr.breach("d", at(3)) {
			t.Error("fired before three new breaches")
		}
		if !tr.breach("d", at(4)) {
			t.Error("did not fire on the third breach after the reset")
		}
	})

	t.Run("cooldown suppresses a quick repeat, then allows one", func(t *testing.T) {
		tr := newBreachTracker()
		for i := 0; i < 3; i++ {
			tr.breach("d", at(i))
		}
		tr.reset("d") // recovered
		for i := 5; i < 8; i++ {
			if tr.breach("d", at(i)) {
				t.Fatal("second alert inside the cooldown")
			}
		}
		// Still breaching once the cooldown has passed: the alert fires.
		if !tr.breach("d", at(2+int(alertCooldown.Minutes())+1)) {
			t.Error("no alert after the cooldown while the breach continues")
		}
	})

	t.Run("devices are independent", func(t *testing.T) {
		tr := newBreachTracker()
		tr.breach("a", at(0))
		tr.breach("a", at(1))
		if tr.breach("b", at(2)) {
			t.Error("b fired using a's streak")
		}
	})
}

func TestBoundCache(t *testing.T) {
	ctx := context.Background()
	clock := time.Date(2026, 10, 20, 12, 30, 0, 0, time.UTC)
	c := newBoundCache()
	c.now = func() time.Time { return clock }
	hour := clock.Truncate(time.Hour)

	loads := 0
	found := false
	load := func(context.Context, string, time.Time) (bound, bool, error) {
		loads++
		return bound{Lower: 1, Upper: 2}, found, nil
	}

	// No bound yet: remembered only briefly, so bounds written later are picked up.
	c.get(ctx, "d", hour, load)
	c.get(ctx, "d", hour, load)
	if loads != 1 {
		t.Fatalf("loads = %d for repeated misses inside the retry window, want 1", loads)
	}
	found = true
	clock = clock.Add(noBoundRetry + time.Second)
	if _, ok, _ := c.get(ctx, "d", hour, load); !ok || loads != 2 {
		t.Fatalf("after the retry window: found=%v loads=%d, want found with a reload", ok, loads)
	}

	// A found bound is reused for boundTTL.
	clock = clock.Add(boundTTL - time.Minute)
	c.get(ctx, "d", hour, load)
	if loads != 2 {
		t.Errorf("loads = %d inside the ttl, want still 2", loads)
	}
	clock = clock.Add(2 * time.Minute)
	c.get(ctx, "d", hour, load)
	if loads != 3 {
		t.Errorf("loads = %d after the ttl, want 3", loads)
	}

	// Errors are not cached.
	boom := func(context.Context, string, time.Time) (bound, bool, error) {
		return bound{}, false, errors.New("db down")
	}
	if _, _, err := c.get(ctx, "other", hour, boom); err == nil {
		t.Error("expected the load error")
	}
}
