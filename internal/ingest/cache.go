package ingest

import (
	"context"
	"sync"
	"time"
)

// device is the slice of a devices row that ingest needs for every reading.
type device struct {
	ID                string
	Name              string // empty if the device has no name
	MeshID            string // mesh_id of the device's group; empty if it has none
	Status            string
	CalibrationEndsAt *time.Time
}

type cacheEntry struct {
	dev     device
	fetched time.Time
}

// deviceCache keeps recently looked-up devices so a reading costs one database
// round trip (the insert) instead of a lookup plus the insert. Entries expire
// after ttl, which bounds how stale a status changed from outside this process
// (for example a manual SQL edit) can be.
type deviceCache struct {
	mu  sync.Mutex
	ttl time.Duration
	now func() time.Time
	m   map[string]cacheEntry
}

func newDeviceCache(ttl time.Duration) *deviceCache {
	return &deviceCache{ttl: ttl, now: time.Now, m: make(map[string]cacheEntry)}
}

// get returns the cached device for mac, calling load on a miss or expired entry.
// Load errors (including unknown devices) are not cached.
func (c *deviceCache) get(ctx context.Context, mac string, load func(context.Context, string) (device, error)) (device, error) {
	c.mu.Lock()
	e, ok := c.m[mac]
	c.mu.Unlock()
	if ok && c.now().Sub(e.fetched) < c.ttl {
		return e.dev, nil
	}

	dev, err := load(ctx, mac)
	if err != nil {
		return device{}, err
	}
	c.mu.Lock()
	c.m[mac] = cacheEntry{dev: dev, fetched: c.now()}
	c.mu.Unlock()
	return dev, nil
}

// invalidate drops mac so the next reading reloads it. Call it whenever this
// process changes a device's row.
func (c *deviceCache) invalidate(mac string) {
	c.mu.Lock()
	delete(c.m, mac)
	c.mu.Unlock()
}
