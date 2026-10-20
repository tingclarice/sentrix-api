// package ingest subscribes to MQTT and writes telemetry to Postgres.
package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tingclarice/sentrix-api/internal/config"
	"github.com/tingclarice/sentrix-api/internal/push"
)

type Service struct {
	cfg      config.Config
	db       *pgxpool.Pool
	devices  *deviceCache
	bounds   *boundCache
	breaches *breachTracker
	notifier Notifier
}

func New(cfg config.Config, db *pgxpool.Pool) *Service {
	return &Service{
		cfg: cfg, db: db,
		devices:  newDeviceCache(15 * time.Second),
		bounds:   newBoundCache(),
		breaches: newBreachTracker(),
	}
}

// Notifier is told about each new alert, for example to send a Web Push. Notify must
// return quickly: it is called on the ingest path.
type Notifier interface {
	Notify(push.Alert)
}

// SetNotifier sets who hears about new alerts. Call it before Connect.
func (s *Service) SetNotifier(n Notifier) { s.notifier = n }

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
				handshakeFilter: s.handleHandshake,
				batchFilter:     s.handleBatch,
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
	h, err := parseHandshake(msg.Topic(), msg.Payload())
	if err != nil {
		log.Printf("bad handshake on %s: %v", msg.Topic(), err)
		return
	}
	if err := s.processHandshake(context.Background(), h); err != nil {
		log.Printf("handshake %s: %v", h.MAC, err)
	}
}

func (s *Service) handleBatch(_ mqtt.Client, msg mqtt.Message) {
	b, rejected, err := parseBatch(msg.Topic(), msg.Payload())
	if err != nil {
		log.Printf("bad telemetry batch on %s: %v", msg.Topic(), err)
		return
	}
	for _, reason := range rejected {
		log.Printf("telemetry batch %s: %s", b.MeshID, reason)
	}
	s.processBatch(context.Background(), b)
}

// processBatch stores every reading, records that those nodes were seen, and marks
// the nodes the root reported as not answering as OFFLINE.
func (s *Service) processBatch(ctx context.Context, b Batch) {
	var seen []string
	for _, r := range b.Readings {
		if err := s.processReading(ctx, r, b.MeshID); err != nil {
			log.Printf("telemetry %s: %v", r.MAC, err)
			continue
		}
		seen = append(seen, r.MAC)
	}
	if len(seen) > 0 {
		if _, err := s.db.Exec(ctx, `UPDATE devices SET last_seen = now() WHERE mac_address = ANY($1)`, seen); err != nil {
			log.Printf("last_seen: %v", err)
		}
	}
	if len(b.OfflineNodes) > 0 {
		s.markOffline(ctx, b.MeshID, b.OfflineNodes)
	}
}

// markOffline sets nodes that are CALIBRATING or ACTIVE to OFFLINE. Only devices in a
// group with this mesh_id (or in no group yet) are touched, so one mesh cannot take
// another's devices offline.
func (s *Service) markOffline(ctx context.Context, meshID string, macs []string) {
	rows, err := s.db.Query(ctx, `
		UPDATE devices SET status = 'OFFLINE'
		WHERE mac_address = ANY($1) AND status IN ('CALIBRATING', 'ACTIVE')
		  AND (group_id IS NULL OR group_id IN (SELECT id FROM groups WHERE mesh_id = $2))
		RETURNING mac_address`, macs, meshID)
	if err != nil {
		log.Printf("mark offline: %v", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var mac string
		if err := rows.Scan(&mac); err == nil {
			s.devices.invalidate(mac)
			log.Printf("device %s is offline", mac)
		}
	}
}

// restoreFromOffline brings an OFFLINE device back. It returns to ACTIVE if the model
// has already written bounds for it, otherwise to CALIBRATING, where the Day 8 check
// will start training once calibration_ends_at has passed.
func (s *Service) restoreFromOffline(ctx context.Context, mac, deviceID string) {
	tag, err := s.db.Exec(ctx, `
		UPDATE devices SET status = (CASE WHEN EXISTS (
			SELECT 1 FROM prediction_bounds b WHERE b.device_id = devices.id)
			THEN 'ACTIVE' ELSE 'CALIBRATING' END)::device_status
		WHERE id = $1 AND status = 'OFFLINE'`, deviceID)
	if err != nil {
		log.Printf("restore %s: %v", mac, err)
		return
	}
	if tag.RowsAffected() > 0 {
		s.devices.invalidate(mac)
		log.Printf("device %s is back online", mac)
	}
}

// processHandshake accepts a node's introduction only if its MAC and PIN match a
// device (and, once the device is in a group, the mesh_id matches that group's).
// The node's reported details and last_seen are then saved. There is no reply to the
// device: a rejected handshake is only logged.
func (s *Service) processHandshake(ctx context.Context, h Handshake) error {
	var deviceID, status string
	err := s.db.QueryRow(ctx, `
		UPDATE devices d SET
		    last_seen = now(),
		    firmware_version = COALESCE(NULLIF($3, ''), d.firmware_version),
		    local_ip = COALESCE(NULLIF($4, ''), d.local_ip),
		    wifi_rssi = COALESCE($5::int, d.wifi_rssi),
		    node_role = COALESCE(NULLIF($6, ''), d.node_role)
		WHERE d.mac_address = $1 AND d.pin_code = $2
		  AND (d.group_id IS NULL OR EXISTS (
		        SELECT 1 FROM groups g WHERE g.id = d.group_id AND g.mesh_id = $7))
		RETURNING d.id, d.status::text`,
		h.MAC, h.PIN, h.FirmwareVersion, h.LocalIP, h.WifiRSSI, h.Role, h.MeshID).Scan(&deviceID, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return errors.New("rejected: unknown device, wrong PIN or wrong mesh_id")
	}
	if err != nil {
		return err
	}
	log.Printf("handshake ok: %s role=%s firmware=%s status=%s", h.MAC, h.Role, h.FirmwareVersion, status)
	if status == "OFFLINE" {
		s.restoreFromOffline(ctx, h.MAC, deviceID)
	}
	return nil
}

func (s *Service) loadDevice(ctx context.Context, mac string) (device, error) {
	var d device
	err := s.db.QueryRow(ctx, `
		SELECT d.id, coalesce(d.name, ''), d.status::text, d.calibration_ends_at, coalesce(g.mesh_id, '')
		FROM devices d LEFT JOIN groups g ON g.id = d.group_id
		WHERE d.mac_address = $1`, mac,
	).Scan(&d.ID, &d.Name, &d.Status, &d.CalibrationEndsAt, &d.MeshID)
	if err != nil {
		return device{}, fmt.Errorf("unknown device: %w", err)
	}
	return d, nil
}

// processReading stores one node's reading. meshID is the mesh the batch came from;
// if the device belongs to a group with a different mesh_id the reading is rejected.
// Pass "" to skip that check.
func (s *Service) processReading(ctx context.Context, r Reading, meshID string) error {
	dev, err := s.devices.get(ctx, r.MAC, s.loadDevice)
	if err != nil {
		return err
	}
	if dev.Status == "UNCLAIMED" {
		return nil
	}
	if meshID != "" && dev.MeshID != "" && dev.MeshID != meshID {
		return fmt.Errorf("mesh_id %q does not match the device's group", meshID)
	}

	_, err = s.db.Exec(ctx, `
		INSERT INTO telemetry (device_id, "timestamp", rms_current, current_temperature, target_temperature)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT DO NOTHING`,
		dev.ID, time.Unix(r.TS, 0), r.RMSCurrent, r.CurrentTemperature, r.TargetTemperature)
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
		if err := s.checkAlert(ctx, dev, r); err != nil {
			log.Printf("alert check %s: %v", r.MAC, err)
		}
	case "OFFLINE":
		s.restoreFromOffline(ctx, r.MAC, dev.ID)
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
