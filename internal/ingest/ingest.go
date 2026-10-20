// package ingest subscribes to MQTT and writes telemetry to Postgres.
package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tingclarice/sentrix-api/internal/config"
)

// macPattern is the canonical MAC format: uppercase, 12 hex, no colons (architecture 5.2).
var macPattern = regexp.MustCompile(`^[0-9A-F]{12}$`)

type Reading struct {
	MAC        string  `json:"mac"`
	RMSCurrent float64 `json:"rms_current"`
	TS         int64   `json:"ts"`
}

// Payload accepts either a single reading or a batched {"readings": [...]},
// since the single-vs-batched decision is still open (architecture 5.4).
type Payload struct {
	Reading
	Readings []Reading `json:"readings"`
}

type Service struct {
	cfg     config.Config
	db      *pgxpool.Pool
	devices *deviceCache
}

func New(cfg config.Config, db *pgxpool.Pool) *Service {
	return &Service{cfg: cfg, db: db, devices: newDeviceCache(15 * time.Second)}
}

// InvalidateDevice drops the cached copy of a device, for when something outside
// ingest (the REST API) has changed its row.
func (s *Service) InvalidateDevice(mac string) { s.devices.invalidate(mac) }

func (s *Service) Connect() (mqtt.Client, error) {
	opts := mqtt.NewClientOptions().
		AddBroker(s.cfg.MQTTBrokerURL).
		SetUsername(s.cfg.MQTTUsername).
		SetPassword(s.cfg.MQTTPassword).
		SetClientID("sentrix-go-backend").
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetKeepAlive(30 * time.Second).
		// Persistent session so QoS 1 messages published while Go is down are delivered on reconnect.
		SetCleanSession(false).
		// Re-subscribe on every (re)connect, otherwise subscriptions are lost after a drop.
		SetOnConnectHandler(func(c mqtt.Client) {
			for topic, h := range map[string]mqtt.MessageHandler{
				"sentrix/handshake":   s.handleHandshake,
				"sentrix/telemetry/+": s.handleTelemetry,
			} {
				if t := c.Subscribe(topic, 1, h); t.Wait() && t.Error() != nil {
					log.Printf("subscribe %s: %v", topic, t.Error())
					continue
				}
				log.Printf("subscribed to %s", topic)
			}
		}).
		SetConnectionLostHandler(func(_ mqtt.Client, err error) {
			log.Printf("MQTT connection lost: %v", err)
		})

	client := mqtt.NewClient(opts)
	if t := client.Connect(); t.Wait() && t.Error() != nil {
		return nil, t.Error()
	}
	return client, nil
}

func (s *Service) handleHandshake(_ mqtt.Client, msg mqtt.Message) {
	// TO DO: verify MAC + PIN against devices and mark the device online (Flow 1, step 8).
	log.Printf("handshake: %s", msg.Payload())
}

func (s *Service) handleTelemetry(_ mqtt.Client, msg mqtt.Message) {
	readings, rejected, err := parseTelemetry(msg.Topic(), msg.Payload())
	if err != nil {
		log.Printf("bad telemetry payload: %v", err)
		return
	}
	for _, reason := range rejected {
		log.Printf("telemetry: %s", reason)
	}
	ctx := context.Background()
	for _, r := range readings {
		if err := s.processReading(ctx, r); err != nil {
			log.Printf("telemetry %s: %v", r.MAC, err)
		}
	}
}

func (s *Service) loadDevice(ctx context.Context, mac string) (device, error) {
	var d device
	err := s.db.QueryRow(ctx,
		`SELECT id, status::text, calibration_ends_at FROM devices WHERE mac_address = $1`, mac,
	).Scan(&d.ID, &d.Status, &d.CalibrationEndsAt)
	if err != nil {
		return device{}, fmt.Errorf("unknown device: %w", err)
	}
	return d, nil
}

func (s *Service) processReading(ctx context.Context, r Reading) error {
	dev, err := s.devices.get(ctx, r.MAC, s.loadDevice)
	if err != nil {
		return err
	}
	if dev.Status == "UNCLAIMED" {
		return nil
	}

	_, err = s.db.Exec(ctx,
		`INSERT INTO telemetry (device_id, "timestamp", rms_current) VALUES ($1, $2, $3)
		 ON CONFLICT DO NOTHING`,
		dev.ID, time.Unix(r.TS, 0), r.RMSCurrent)
	if err != nil {
		return err
	}

	switch dev.Status {
	case "CALIBRATING":
		// Only hit the database once the cached deadline has passed.
		if dev.CalibrationEndsAt != nil && !dev.CalibrationEndsAt.After(time.Now()) {
			s.maybeStartCalibration(ctx, r.MAC, dev.ID)
		}
	case "ACTIVE":
		// TO DO: check bounds, alert after 3 consecutive breaches (architecture 8.3).
	}
	return nil
}

// maybeStartCalibration flips CALIBRATING -> ACTIVE atomically; only one message
// can win the UPDATE, so Python is never triggered twice.
func (s *Service) maybeStartCalibration(ctx context.Context, mac, deviceID string) {
	tag, err := s.db.Exec(ctx, `
		UPDATE devices SET status = 'ACTIVE'
		WHERE id = $1 AND status = 'CALIBRATING' AND calibration_ends_at <= now()`, deviceID)
	if err != nil || tag.RowsAffected() == 0 {
		return
	}
	s.devices.invalidate(mac)
	go func() {
		if err := s.triggerPython(deviceID); err != nil {
			log.Printf("python trigger failed for %s: %v, reverting", deviceID, err)
			s.db.Exec(context.Background(),
				`UPDATE devices SET status = 'CALIBRATING' WHERE id = $1`, deviceID)
			s.devices.invalidate(mac)
		}
	}()
}

func (s *Service) triggerPython(deviceID string) error {
	body, _ := json.Marshal(map[string]string{"device_id": deviceID})
	req, err := http.NewRequest("POST", s.cfg.PythonServiceURL+"/api/calibrate", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Key", s.cfg.InternalAPIKey)

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("python returned %d", resp.StatusCode)
	}
	return nil
}
