//go:build integration

package ingest

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tingclarice/sentrix-api/internal/config"
)

// Needs the device_presence_and_temperature migration. Run with:
//
//	go test -tags integration ./internal/ingest
//
// Creates a throwaway group and three devices and deletes them afterwards.
// Never point it at production.
func TestBatchHandshakeAndOfflineAgainstRealDatabase(t *testing.T) {
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

	var userID, groupID string
	if err := db.QueryRow(ctx, `SELECT id FROM users LIMIT 1`).Scan(&userID); err != nil {
		t.Skipf("no user to borrow: %v", err)
	}
	const meshID = "DEADBEEF0005"
	macs := []string{"0DEADBEEF011", "0DEADBEEF012", "0DEADBEEF013"} // root, child, a child that goes offline
	t.Cleanup(func() {
		db.Exec(ctx, `DELETE FROM devices WHERE mac_address LIKE '0DEADBEEF01_'`)
		db.Exec(ctx, `DELETE FROM groups WHERE mesh_id = $1`, meshID)
	})
	db.Exec(ctx, `DELETE FROM devices WHERE mac_address LIKE '0DEADBEEF01_'`)
	db.Exec(ctx, `DELETE FROM groups WHERE mesh_id = $1`, meshID)
	if err := db.QueryRow(ctx,
		`INSERT INTO groups (user_id, name, mesh_id) VALUES ($1, 'batch test', $2) RETURNING id`, userID, meshID).
		Scan(&groupID); err != nil {
		t.Fatal(err)
	}
	for _, mac := range macs {
		if _, err := db.Exec(ctx,
			`INSERT INTO devices (mac_address, pin_code, group_id, status) VALUES ($1, '849201', $2, 'ACTIVE')`,
			mac, groupID); err != nil {
			t.Fatal(err)
		}
	}
	status := func(mac string) string {
		var s string
		db.QueryRow(ctx, `SELECT status::text FROM devices WHERE mac_address = $1`, mac).Scan(&s)
		return s
	}
	svc := New(config.Config{}, db)
	now := time.Now().Unix()
	colon := func(mac string) string { // AABBCCDDEEFF -> AA:BB:CC:DD:EE:FF, as the firmware sends it
		var parts []string
		for i := 0; i < len(mac); i += 2 {
			parts = append(parts, mac[i:i+2])
		}
		return strings.Join(parts, ":")
	}

	// --- Handshake: the right PIN is accepted and its details are stored.
	err = svc.processHandshake(ctx, Handshake{MAC: macs[0], PIN: "849201", MeshID: meshID, Role: "root",
		FirmwareVersion: "1.2.4", LocalIP: "192.168.1.45", WifiRSSI: intPtr(-62)})
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	var fw, ip, role string
	var rssi int
	var seen *time.Time
	if err := db.QueryRow(ctx, `SELECT firmware_version, local_ip, wifi_rssi, node_role, last_seen FROM devices WHERE mac_address = $1`, macs[0]).
		Scan(&fw, &ip, &rssi, &role, &seen); err != nil {
		t.Fatal(err)
	}
	if fw != "1.2.4" || ip != "192.168.1.45" || rssi != -62 || role != "root" || seen == nil {
		t.Errorf("stored handshake = %s %s %d %s %v", fw, ip, rssi, role, seen)
	}
	// A later handshake without optional fields keeps what was stored.
	if err := svc.processHandshake(ctx, Handshake{MAC: macs[0], PIN: "849201", MeshID: meshID}); err != nil {
		t.Fatal(err)
	}
	db.QueryRow(ctx, `SELECT firmware_version FROM devices WHERE mac_address = $1`, macs[0]).Scan(&fw)
	if fw != "1.2.4" {
		t.Errorf("firmware = %q after a bare handshake, want it kept", fw)
	}
	// Wrong PIN, wrong mesh and unknown MAC are all rejected.
	for name, h := range map[string]Handshake{
		"wrong pin":  {MAC: macs[0], PIN: "000000", MeshID: meshID, FirmwareVersion: "evil"},
		"wrong mesh": {MAC: macs[0], PIN: "849201", MeshID: "OTHERMESH", FirmwareVersion: "evil"},
		"no device":  {MAC: "0DEADBEEF0FF", PIN: "849201", MeshID: meshID},
	} {
		if err := svc.processHandshake(ctx, h); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	db.QueryRow(ctx, `SELECT firmware_version FROM devices WHERE mac_address = $1`, macs[0]).Scan(&fw)
	if fw != "1.2.4" {
		t.Errorf("a rejected handshake changed firmware to %q", fw)
	}

	// --- Batch: readings stored with temperatures; last_seen updated; one node offline.
	parse := func(payload string) Batch {
		t.Helper()
		b, _, err := parseBatch("sentrix/dt/"+meshID+"/batch", []byte(payload))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	batch1 := parse(`{"mesh_id":"` + meshID + `","timestamp":` + itoa(now-120) + `,"readings":[
	  {"mac":"` + colon(macs[0]) + `","node_type":"root","rms_current":4.5,"current_temperature":38.5,"target_temperature":45},
	  {"mac":"` + colon(macs[1]) + `","node_type":"child","rms_current":1.8}],
	  "offline_nodes":["` + colon(macs[2]) + `"]}`)
	svc.processBatch(ctx, batch1)

	var amps, cur float64
	var tgt *float64
	if err := db.QueryRow(ctx, `SELECT t.rms_current, t.current_temperature, t.target_temperature FROM telemetry t
		JOIN devices d ON d.id = t.device_id WHERE d.mac_address = $1`, macs[0]).Scan(&amps, &cur, &tgt); err != nil {
		t.Fatalf("reading for the root: %v", err)
	}
	if amps != 4.5 || cur != 38.5 || tgt == nil || *tgt != 45 {
		t.Errorf("root reading = %v %v %v", amps, cur, tgt)
	}
	var childTemp *float64
	db.QueryRow(ctx, `SELECT t.current_temperature FROM telemetry t JOIN devices d ON d.id = t.device_id
		WHERE d.mac_address = $1`, macs[1]).Scan(&childTemp)
	if childTemp != nil {
		t.Errorf("a reading without temperatures stored %v, want NULL", *childTemp)
	}
	if got := status(macs[2]); got != "OFFLINE" {
		t.Errorf("node in offline_nodes is %s, want OFFLINE", got)
	}
	if got := status(macs[0]); got != "ACTIVE" {
		t.Errorf("reporting node is %s, want ACTIVE", got)
	}
	db.QueryRow(ctx, `SELECT last_seen FROM devices WHERE mac_address = $1`, macs[1]).Scan(&seen)
	if seen == nil || time.Since(*seen) > time.Minute {
		t.Errorf("last_seen = %v, want just now", seen)
	}

	// A second copy of the same batch adds nothing (QoS 1 redelivery).
	svc.processBatch(ctx, batch1)
	var n int
	db.QueryRow(ctx, `SELECT count(*) FROM telemetry t JOIN devices d ON d.id = t.device_id WHERE d.mac_address LIKE '0DEADBEEF01_'`).Scan(&n)
	if n != 2 {
		t.Errorf("%d telemetry rows after a redelivered batch, want 2", n)
	}

	// --- A different mesh cannot publish for these devices or take them offline.
	other := Batch{MeshID: "SOMEONEELSE", TS: now - 60,
		Readings:     []Reading{{MAC: macs[1], RMSCurrent: 9, TS: now - 60}},
		OfflineNodes: []string{macs[0]}}
	svc.processBatch(ctx, other)
	db.QueryRow(ctx, `SELECT count(*) FROM telemetry t JOIN devices d ON d.id = t.device_id WHERE d.mac_address = $1`, macs[1]).Scan(&n)
	if n != 1 {
		t.Errorf("a batch from another mesh stored a reading (%d rows for the child)", n)
	}
	if got := status(macs[0]); got != "ACTIVE" {
		t.Errorf("another mesh took a device %s", got)
	}

	// --- Coming back: no bounds exist yet, so CALIBRATING (not ACTIVE).
	svc.processBatch(ctx, parse(`{"mesh_id":"`+meshID+`","timestamp":`+itoa(now-60)+`,"readings":[
	  {"mac":"`+colon(macs[2])+`","rms_current":2.0}]}`))
	if got := status(macs[2]); got != "CALIBRATING" {
		t.Errorf("returning node with no bounds is %s, want CALIBRATING", got)
	}

	// With bounds written, a returning node goes straight back to ACTIVE.
	if _, err := db.Exec(ctx, `UPDATE devices SET status = 'OFFLINE' WHERE mac_address = $1`, macs[2]); err != nil {
		t.Fatal(err)
	}
	svc.devices.invalidate(macs[2])
	if _, err := db.Exec(ctx, `INSERT INTO prediction_bounds (device_id, "timestamp", upper_bound, lower_bound)
		SELECT id, date_trunc('hour', now()), 7, 1 FROM devices WHERE mac_address = $1`, macs[2]); err != nil {
		t.Fatal(err)
	}
	if err := svc.processHandshake(ctx, Handshake{MAC: macs[2], PIN: "849201", MeshID: meshID}); err != nil {
		t.Fatal(err)
	}
	if got := status(macs[2]); got != "ACTIVE" {
		t.Errorf("a handshake from an OFFLINE node with bounds leaves it %s, want ACTIVE", got)
	}
}

func intPtr(v int) *int { return &v }

func itoa(v int64) string { return strconv.FormatInt(v, 10) }
