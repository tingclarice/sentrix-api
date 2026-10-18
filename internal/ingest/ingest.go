// Package ingest subscribes to MQTT and writes telemetry to Postgres.
package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
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
	cfg config.Config
	db  *pgxpool.Pool
}

func New(cfg config.Config, db *pgxpool.Pool) *Service {
	return &Service{cfg: cfg, db: db}
}

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
	// TODO: verify MAC + PIN against devices and mark the device online (Flow 1, step 8).
	log.Printf("handshake: %s", msg.Payload())
}

func (s *Service) handleTelemetry(_ mqtt.Client, msg mqtt.Message) {
	var p Payload
	if err := json.Unmarshal(msg.Payload(), &p); err != nil {
		log.Printf("bad telemetry payload: %v", err)
		return
	}
	readings := p.Readings
	batched := len(readings) > 0
	if !batched {
		readings = []Reading{p.Reading}
	}
	// Topic is sentrix/telemetry/{mac}. Single readings must match it; batches come from
	// the mesh root and carry leaf MACs, so they are exempt.
	topicMAC := strings.ToUpper(msg.Topic()[strings.LastIndex(msg.Topic(), "/")+1:])
	ctx := context.Background()
	for _, r := range readings {
		r.MAC = strings.ToUpper(r.MAC)
		if !macPattern.MatchString(r.MAC) {
			log.Printf("telemetry: invalid mac %q", r.MAC)
			continue
		}
		if !batched && r.MAC != topicMAC {
			log.Printf("telemetry: payload mac %s does not match topic mac %s", r.MAC, topicMAC)
			continue
		}
		if r.TS <= 0 {
			log.Printf("telemetry %s: missing or invalid ts", r.MAC)
			continue
		}
		if err := s.processReading(ctx, r); err != nil {
			log.Printf("telemetry %s: %v", r.MAC, err)
		}
	}
}

func (s *Service) processReading(ctx context.Context, r Reading) error {
	var deviceID, status string
	err := s.db.QueryRow(ctx,
		`SELECT id, status::text FROM devices WHERE mac_address = $1`, r.MAC,
	).Scan(&deviceID, &status)
	if err != nil {
		return fmt.Errorf("unknown device: %w", err)
	}
	if status == "UNCLAIMED" {
		return nil
	}

	_, err = s.db.Exec(ctx,
		`INSERT INTO telemetry (device_id, "timestamp", rms_current) VALUES ($1, $2, $3)
		 ON CONFLICT DO NOTHING`,
		deviceID, time.Unix(r.TS, 0), r.RMSCurrent)
	if err != nil {
		return err
	}

	switch status {
	case "CALIBRATING":
		s.maybeStartCalibration(ctx, deviceID)
	case "ACTIVE":
		// TODO: check bounds, alert after 3 consecutive breaches (architecture 8.3).
	}
	return nil
}

// maybeStartCalibration flips CALIBRATING -> ACTIVE atomically; only one message
// can win the UPDATE, so Python is never triggered twice.
func (s *Service) maybeStartCalibration(ctx context.Context, deviceID string) {
	tag, err := s.db.Exec(ctx, `
		UPDATE devices SET status = 'ACTIVE'
		WHERE id = $1 AND status = 'CALIBRATING' AND calibration_ends_at <= now()`, deviceID)
	if err != nil || tag.RowsAffected() == 0 {
		return
	}
	go func() {
		if err := s.triggerPython(deviceID); err != nil {
			log.Printf("python trigger failed for %s: %v, reverting", deviceID, err)
			s.db.Exec(context.Background(),
				`UPDATE devices SET status = 'CALIBRATING' WHERE id = $1`, deviceID)
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
