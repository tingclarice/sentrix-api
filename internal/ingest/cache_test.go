package ingest

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDeviceCache(t *testing.T) {
	ctx := context.Background()
	clock := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	c := newDeviceCache(15 * time.Second)
	c.now = func() time.Time { return clock }

	loads := 0
	load := func(_ context.Context, mac string) (device, error) {
		loads++
		return device{ID: "id-" + mac, Status: "CALIBRATING"}, nil
	}

	for i := 0; i < 3; i++ {
		if _, err := c.get(ctx, "AABBCCDDEEFF", load); err != nil {
			t.Fatal(err)
		}
	}
	if loads != 1 {
		t.Fatalf("loads = %d after 3 gets inside the ttl, want 1", loads)
	}

	clock = clock.Add(16 * time.Second)
	c.get(ctx, "AABBCCDDEEFF", load)
	if loads != 2 {
		t.Fatalf("loads = %d after the ttl expired, want 2", loads)
	}

	c.invalidate("AABBCCDDEEFF")
	c.get(ctx, "AABBCCDDEEFF", load)
	if loads != 3 {
		t.Fatalf("loads = %d after invalidate, want 3", loads)
	}
}

func TestDeviceCacheDoesNotCacheErrors(t *testing.T) {
	ctx := context.Background()
	c := newDeviceCache(time.Minute)

	loads := 0
	load := func(context.Context, string) (device, error) {
		loads++
		return device{}, errors.New("unknown device")
	}

	for i := 0; i < 2; i++ {
		if _, err := c.get(ctx, "112233445566", load); err == nil {
			t.Fatal("expected an error")
		}
	}
	if loads != 2 {
		t.Fatalf("loads = %d, want 2 (a device claimed later must be found)", loads)
	}
}
