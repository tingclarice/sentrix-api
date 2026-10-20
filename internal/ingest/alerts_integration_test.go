//go:build integration

package ingest

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tingclarice/sentrix-api/internal/config"
)

// Run against a dev database with: go test -tags integration ./internal/ingest
// Creates a throwaway ACTIVE device with bounds, feeds it readings through the real
// ingest path, and deletes it afterwards. Never point it at production.
func TestAlertFlowAgainstRealDatabase(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	pc, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	pc.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	db, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)

	const mac = "0DEADBEEF002"
	var deviceID string
	db.Exec(ctx, `DELETE FROM devices WHERE mac_address = $1`, mac)
	if err := db.QueryRow(ctx,
		`INSERT INTO devices (mac_address, pin_code, status) VALUES ($1, '654321', 'ACTIVE') RETURNING id`, mac).
		Scan(&deviceID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec(ctx, `DELETE FROM devices WHERE id = $1`, deviceID) }) // cascades

	// Bounds 4-7A for this hour and the previous one, in case the test straddles an hour.
	hour := time.Now().Truncate(time.Hour)
	for _, h := range []time.Time{hour, hour.Add(-time.Hour)} {
		if _, err := db.Exec(ctx,
			`INSERT INTO prediction_bounds (device_id, "timestamp", upper_bound, lower_bound) VALUES ($1, $2, 7, 4)`,
			deviceID, h); err != nil {
			t.Fatal(err)
		}
	}

	svc := New(config.Config{}, db)
	now := time.Now()
	n := 0 // minutes before now, so timestamps are distinct and ascending
	feed := func(minsAgo int, amps float64) {
		t.Helper()
		r := Reading{MAC: mac, RMSCurrent: amps, TS: now.Add(-time.Duration(minsAgo) * time.Minute).Unix()}
		if err := svc.processReading(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	alerts := func() (count int, severity string) {
		db.QueryRow(ctx, `SELECT count(*), coalesce(max(severity::text), '') FROM alerts WHERE device_id = $1`, deviceID).
			Scan(&count, &severity)
		return
	}

	// Normal readings, then two breaches: nothing yet.
	n = 9
	feed(n, 5)
	n--
	feed(n, 5.5)
	n--
	feed(n, 15)
	n--
	feed(n, 15)
	n--
	if c, _ := alerts(); c != 0 {
		t.Fatalf("%d alerts after 2 breaches, want 0", c)
	}

	// The third breach in a row raises one CRITICAL alert (15A vs 4-7A).
	feed(n, 15)
	n--
	c, sev := alerts()
	if c != 1 || sev != "CRITICAL" {
		t.Fatalf("after 3 breaches: %d alerts, severity %q, want 1 CRITICAL", c, sev)
	}

	// The anomaly continues: still one alert.
	feed(n, 15)
	n--
	feed(n, 15)
	n--
	if c, _ := alerts(); c != 1 {
		t.Errorf("%d alerts while the same anomaly continues, want 1", c)
	}

	// A standby reading resets the streak and is never an alert on its own.
	feed(n, 0.02)
	if c, _ := alerts(); c != 1 {
		t.Errorf("%d alerts after a standby reading, want 1", c)
	}

	// Stored values match what the engine saw.
	var actual, max, min float64
	if err := db.QueryRow(ctx,
		`SELECT actual_value, expected_max, expected_min FROM alerts WHERE device_id = $1`, deviceID).
		Scan(&actual, &max, &min); err != nil {
		t.Fatal(err)
	}
	if actual != 15 || max != 7 || min != 4 {
		t.Errorf("alert row = %v / %v-%v, want 15 / 4-7", actual, min, max)
	}

	// Old readings are stored but never alerted on, even for a fresh device.
	svc2 := New(config.Config{}, db)
	for i := 0; i < 5; i++ {
		r := Reading{MAC: mac, RMSCurrent: 15, TS: now.Add(-time.Duration(120+i) * time.Minute).Unix()}
		if err := svc2.processReading(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if c, _ := alerts(); c != 1 {
		t.Errorf("%d alerts after replaying old readings, want still 1", c)
	}
	var stored int
	db.QueryRow(ctx, `SELECT count(*) FROM telemetry WHERE device_id = $1`, deviceID).Scan(&stored)
	if stored != 13 { // 7 live + 1 standby + 5 old
		t.Errorf("%d telemetry rows stored, want 13", stored)
	}
}
