//go:build integration

package store

import (
	"context"
	"errors"
	"os"
	"regexp"
	"testing"

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
}
