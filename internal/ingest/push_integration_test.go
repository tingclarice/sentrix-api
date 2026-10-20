//go:build integration

package ingest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/SherClockHolmes/webpush-go"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tingclarice/sentrix-api/internal/config"
	"github.com/tingclarice/sentrix-api/internal/push"
	"github.com/tingclarice/sentrix-api/internal/pushtest"
	"github.com/tingclarice/sentrix-api/internal/store"
)

// Run against a dev database with: go test -tags integration ./internal/ingest
// End to end: readings -> alert engine -> alert row -> push sender -> a fake browser
// push service, where the "browser" decrypts the notification. Uses a throwaway group,
// device and subscription, deleted afterwards. Never point it at production.
func TestAlertSendsPushEndToEnd(t *testing.T) {
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

	var userID string
	if err := db.QueryRow(ctx, `SELECT id FROM users LIMIT 1`).Scan(&userID); err != nil {
		t.Skipf("no user to borrow: %v", err)
	}

	// The fake push service records the first request, then the test decrypts it.
	browser := pushtest.NewBrowser(t)
	got := make(chan []byte, 4)
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- body
		w.WriteHeader(http.StatusCreated)
	}))
	defer svc.Close()
	endpoint := svc.URL + "/e2e-" + time.Now().Format("150405.000")

	const mac = "0DEADBEEF004"
	var groupID, deviceID string
	db.Exec(ctx, `DELETE FROM devices WHERE mac_address = $1`, mac)
	if err := db.QueryRow(ctx,
		`INSERT INTO groups (user_id, name, mesh_id) VALUES ($1, 'push e2e', 'DEADBEEF0004') RETURNING id`, userID).
		Scan(&groupID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Exec(ctx, `DELETE FROM push_subscriptions WHERE endpoint = $1`, endpoint)
		db.Exec(ctx, `DELETE FROM devices WHERE mac_address = $1`, mac)
		db.Exec(ctx, `DELETE FROM groups WHERE id = $1`, groupID)
	})
	if err := db.QueryRow(ctx,
		`INSERT INTO devices (mac_address, pin_code, name, group_id, status)
		 VALUES ($1, '654321', 'Pump E2E', $2, 'ACTIVE') RETURNING id`, mac, groupID).Scan(&deviceID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx,
		`INSERT INTO push_subscriptions (user_id, endpoint, p256dh, auth) VALUES ($1, $2, $3, $4)`,
		userID, endpoint, browser.P256dh, browser.Auth); err != nil {
		t.Fatal(err)
	}
	hour := time.Now().Truncate(time.Hour)
	for _, h := range []time.Time{hour, hour.Add(-time.Hour)} {
		if _, err := db.Exec(ctx,
			`INSERT INTO prediction_bounds (device_id, "timestamp", upper_bound, lower_bound) VALUES ($1, $2, 7, 4)`,
			deviceID, h); err != nil {
			t.Fatal(err)
		}
	}

	priv, pub, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		t.Fatal(err)
	}
	ing := New(config.Config{}, db)
	ing.SetNotifier(push.New(store.NewPG(db), pub, priv, "mailto:test@example.com"))

	feed := func(minsAgo int, amps float64) {
		t.Helper()
		r := Reading{MAC: mac, RMSCurrent: amps, TS: time.Now().Add(-time.Duration(minsAgo) * time.Minute).Unix()}
		if err := ing.processReading(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	// Two breaches: no alert, so no push.
	feed(5, 15)
	feed(4, 15)
	select {
	case <-got:
		t.Fatal("a push was sent before the third breach")
	case <-time.After(300 * time.Millisecond):
	}

	// The third breach raises the alert and the owner's browser gets a push.
	feed(3, 15)
	var body []byte
	select {
	case body = <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("no push arrived after the alert")
	}
	plain, err := browser.Decrypt(body)
	if err != nil {
		t.Fatalf("browser could not decrypt the push: %v", err)
	}
	var p struct {
		Title    string `json:"title"`
		Body     string `json:"body"`
		AlertID  string `json:"alert_id"`
		DeviceID string `json:"device_id"`
		Severity string `json:"severity"`
	}
	if err := json.Unmarshal(plain, &p); err != nil {
		t.Fatalf("payload %s: %v", plain, err)
	}
	if p.Title != "CRITICAL alert: Pump E2E" || p.Body != "15.00A, expected 4.00-7.00A" ||
		p.DeviceID != deviceID || p.Severity != "CRITICAL" {
		t.Errorf("push payload = %+v", p)
	}
	var stored string
	if err := db.QueryRow(ctx, `SELECT id FROM alerts WHERE device_id = $1`, deviceID).Scan(&stored); err != nil || stored != p.AlertID {
		t.Errorf("alert row %q (%v) does not match the push's alert_id %q", stored, err, p.AlertID)
	}

	// The anomaly continues: no second push.
	feed(2, 15)
	select {
	case <-got:
		t.Error("a second push was sent for the same anomaly")
	case <-time.After(300 * time.Millisecond):
	}
}
