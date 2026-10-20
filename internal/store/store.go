// Package store holds the database queries behind the REST API.
package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound means no row matched. For ownership checks it also covers "exists but
// belongs to someone else", so callers cannot probe for other users' resources.
var ErrNotFound = errors.New("not found")

type Group struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description *string   `json:"description"`
	MeshID      string    `json:"mesh_id"`
	CreatedAt   time.Time `json:"created_at"`
}

// Device deliberately has no PIN field: the PIN must never leave the server.
type Device struct {
	ID                   string     `json:"id"`
	MACAddress           string     `json:"mac_address"`
	Name                 *string    `json:"name"`
	GroupID              *string    `json:"group_id"`
	Status               string     `json:"status"`
	CalibrationStartedAt *time.Time `json:"calibration_started_at"`
	CalibrationEndsAt    *time.Time `json:"calibration_ends_at"`
	CreatedAt            time.Time  `json:"created_at"`
}

// TelemetryPoint is one chart point. Raw readings have MaxRMSCurrent equal to
// RMSCurrent; bucketed points carry the bucket's average and its peak, so a short
// spike is still visible in a long range.
type TelemetryPoint struct {
	Timestamp     time.Time `json:"timestamp"`
	RMSCurrent    float64   `json:"rms_current"`
	MaxRMSCurrent float64   `json:"max_rms_current"`
}

type Bound struct {
	Timestamp  time.Time `json:"timestamp"`
	UpperBound float64   `json:"upper_bound"`
	LowerBound float64   `json:"lower_bound"`
}

// Summary is the dashboard overview for one user.
type Summary struct {
	Groups           int `json:"groups"`
	Devices          int `json:"devices"`
	Calibrating      int `json:"calibrating"`
	Active           int `json:"active"`
	Offline          int `json:"offline"`
	Anomalies24h     int `json:"anomalies_24h"`
	UnresolvedAlerts int `json:"unresolved_alerts"`
}

// Store is what the HTTP handlers need from the database.
type Store interface {
	CreateGroup(ctx context.Context, userID, name string, description *string) (Group, error)
	ListGroups(ctx context.Context, userID string) ([]Group, error)
	// GroupOwnedBy returns ErrNotFound unless the group exists and belongs to userID.
	GroupOwnedBy(ctx context.Context, groupID, userID string) error
	// ClaimDevice attaches an UNCLAIMED device with a matching MAC and PIN to the group
	// and starts calibration. It returns ErrNotFound if nothing matches.
	ClaimDevice(ctx context.Context, groupID, mac, pin string, name *string, calibrationMinutes int) (Device, error)
	ListDevices(ctx context.Context, groupID string) ([]Device, error)

	// GetGroup returns ErrNotFound unless the group exists and belongs to userID.
	GetGroup(ctx context.Context, groupID, userID string) (Group, error)
	// DeviceOwnedBy returns ErrNotFound unless the device is in one of userID's groups.
	DeviceOwnedBy(ctx context.Context, deviceID, userID string) error
	// Telemetry returns readings since the given time. bucket == 0 returns raw
	// readings; otherwise they are averaged into buckets of that size.
	Telemetry(ctx context.Context, deviceID string, since time.Time, bucket time.Duration) ([]TelemetryPoint, error)
	// Bounds returns predicted bounds from the given time onwards, including the
	// forecast hours that are still in the future.
	Bounds(ctx context.Context, deviceID string, from time.Time) ([]Bound, error)
	Summary(ctx context.Context, userID string) (Summary, error)
}

type PG struct{ db *pgxpool.Pool }

func NewPG(db *pgxpool.Pool) *PG { return &PG{db: db} }

const groupCols = `id, name, description, mesh_id, created_at`

func scanGroup(row pgx.Row) (Group, error) {
	var g Group
	err := row.Scan(&g.ID, &g.Name, &g.Description, &g.MeshID, &g.CreatedAt)
	return g, err
}

// newMeshID returns a random 6-byte mesh ID as 12 uppercase hex characters,
// the format the groups table requires.
func newMeshID() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return strings.ToUpper(hex.EncodeToString(b)), nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func (s *PG) CreateGroup(ctx context.Context, userID, name string, description *string) (Group, error) {
	// mesh_id is unique; on the (very unlikely) collision, try a new one.
	for range 5 {
		mesh, err := newMeshID()
		if err != nil {
			return Group{}, err
		}
		g, err := scanGroup(s.db.QueryRow(ctx,
			`INSERT INTO groups (user_id, name, description, mesh_id) VALUES ($1, $2, $3, $4)
			 RETURNING `+groupCols, userID, name, description, mesh))
		if err == nil {
			return g, nil
		}
		if !isUniqueViolation(err) {
			return Group{}, err
		}
	}
	return Group{}, errors.New("could not generate a unique mesh id")
}

func (s *PG) ListGroups(ctx context.Context, userID string) ([]Group, error) {
	rows, err := s.db.Query(ctx,
		`SELECT `+groupCols+` FROM groups WHERE user_id = $1 ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	groups := []Group{}
	for rows.Next() {
		g, err := scanGroup(rows)
		if err != nil {
			return nil, err
		}
		groups = append(groups, g)
	}
	return groups, rows.Err()
}

func (s *PG) GroupOwnedBy(ctx context.Context, groupID, userID string) error {
	var one int
	err := s.db.QueryRow(ctx,
		`SELECT 1 FROM groups WHERE id = $1 AND user_id = $2`, groupID, userID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

const deviceCols = `id, mac_address, name, group_id, status::text,
	calibration_started_at, calibration_ends_at, created_at`

func scanDevice(row pgx.Row) (Device, error) {
	var d Device
	err := row.Scan(&d.ID, &d.MACAddress, &d.Name, &d.GroupID, &d.Status,
		&d.CalibrationStartedAt, &d.CalibrationEndsAt, &d.CreatedAt)
	return d, err
}

func (s *PG) ClaimDevice(ctx context.Context, groupID, mac, pin string, name *string, calibrationMinutes int) (Device, error) {
	// One atomic statement: the MAC, PIN and UNCLAIMED checks and the state change
	// happen together, so two people cannot claim the same device.
	d, err := scanDevice(s.db.QueryRow(ctx, `
		UPDATE devices
		SET group_id = $1,
		    name = COALESCE($4, name),
		    status = 'CALIBRATING',
		    calibration_started_at = now(),
		    calibration_ends_at = now() + make_interval(mins => $5::int)
		WHERE mac_address = $2 AND pin_code = $3 AND status = 'UNCLAIMED'
		RETURNING `+deviceCols, groupID, mac, pin, name, calibrationMinutes))
	if errors.Is(err, pgx.ErrNoRows) {
		return Device{}, ErrNotFound
	}
	return d, err
}

func (s *PG) ListDevices(ctx context.Context, groupID string) ([]Device, error) {
	rows, err := s.db.Query(ctx,
		`SELECT `+deviceCols+` FROM devices WHERE group_id = $1 ORDER BY created_at`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	devices := []Device{}
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		devices = append(devices, d)
	}
	return devices, rows.Err()
}

func (s *PG) GetGroup(ctx context.Context, groupID, userID string) (Group, error) {
	g, err := scanGroup(s.db.QueryRow(ctx,
		`SELECT `+groupCols+` FROM groups WHERE id = $1 AND user_id = $2`, groupID, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Group{}, ErrNotFound
	}
	return g, err
}

func (s *PG) DeviceOwnedBy(ctx context.Context, deviceID, userID string) error {
	var one int
	err := s.db.QueryRow(ctx, `
		SELECT 1 FROM devices d JOIN groups g ON g.id = d.group_id
		WHERE d.id = $1 AND g.user_id = $2`, deviceID, userID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func (s *PG) Telemetry(ctx context.Context, deviceID string, since time.Time, bucket time.Duration) ([]TelemetryPoint, error) {
	var (
		rows pgx.Rows
		err  error
	)
	if bucket <= 0 {
		rows, err = s.db.Query(ctx, `
			SELECT "timestamp", rms_current, rms_current FROM telemetry
			WHERE device_id = $1 AND "timestamp" >= $2 ORDER BY "timestamp"`, deviceID, since)
	} else {
		rows, err = s.db.Query(ctx, `
			SELECT date_bin(make_interval(secs => $3::int), "timestamp", TIMESTAMPTZ '2001-01-01') AS ts,
			       avg(rms_current), max(rms_current)
			FROM telemetry
			WHERE device_id = $1 AND "timestamp" >= $2
			GROUP BY ts ORDER BY ts`, deviceID, since, int(bucket.Seconds()))
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	points := []TelemetryPoint{}
	for rows.Next() {
		var p TelemetryPoint
		if err := rows.Scan(&p.Timestamp, &p.RMSCurrent, &p.MaxRMSCurrent); err != nil {
			return nil, err
		}
		points = append(points, p)
	}
	return points, rows.Err()
}

func (s *PG) Bounds(ctx context.Context, deviceID string, from time.Time) ([]Bound, error) {
	rows, err := s.db.Query(ctx, `
		SELECT "timestamp", upper_bound, lower_bound FROM prediction_bounds
		WHERE device_id = $1 AND "timestamp" >= $2 ORDER BY "timestamp"`, deviceID, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	bounds := []Bound{}
	for rows.Next() {
		var b Bound
		if err := rows.Scan(&b.Timestamp, &b.UpperBound, &b.LowerBound); err != nil {
			return nil, err
		}
		bounds = append(bounds, b)
	}
	return bounds, rows.Err()
}

func (s *PG) Summary(ctx context.Context, userID string) (Summary, error) {
	var sum Summary
	err := s.db.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM groups WHERE user_id = $1),
		  count(d.id),
		  count(d.id) FILTER (WHERE d.status = 'CALIBRATING'),
		  count(d.id) FILTER (WHERE d.status = 'ACTIVE'),
		  count(d.id) FILTER (WHERE d.status = 'OFFLINE')
		FROM devices d JOIN groups g ON g.id = d.group_id
		WHERE g.user_id = $1`, userID).
		Scan(&sum.Groups, &sum.Devices, &sum.Calibrating, &sum.Active, &sum.Offline)
	if err != nil {
		return Summary{}, err
	}
	err = s.db.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE a."timestamp" >= now() - interval '24 hours'),
		       count(*) FILTER (WHERE NOT a.is_resolved)
		FROM alerts a
		JOIN devices d ON d.id = a.device_id
		JOIN groups g ON g.id = d.group_id
		WHERE g.user_id = $1`, userID).
		Scan(&sum.Anomalies24h, &sum.UnresolvedAlerts)
	return sum, err
}
