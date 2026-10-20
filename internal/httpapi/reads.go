package httpapi

import (
	"errors"
	"net/http"
	"regexp"
	"time"

	"github.com/tingclarice/sentrix-api/internal/store"
)

// uuidPattern guards path IDs before they reach Postgres, which rejects a malformed
// uuid with an error that would otherwise surface as a 500.
var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// chartRange describes one of the supported ?range= values. Longer ranges are
// averaged into buckets so a 7-day chart is a few hundred points, not 10 080.
type chartRange struct {
	window time.Duration
	bucket time.Duration // 0 = raw readings
}

var chartRanges = map[string]chartRange{
	"1h":  {time.Hour, 0},
	"24h": {24 * time.Hour, 5 * time.Minute},
	"7d":  {7 * 24 * time.Hour, 15 * time.Minute},
}

const defaultRange = "24h"

func parseRange(w http.ResponseWriter, r *http.Request) (string, chartRange, bool) {
	name := r.URL.Query().Get("range")
	if name == "" {
		name = defaultRange
	}
	cr, ok := chartRanges[name]
	if !ok {
		writeError(w, http.StatusBadRequest, "range must be 1h, 24h or 7d")
	}
	return name, cr, ok
}

// ownedDevice checks that the {id} device is in one of the user's groups. A device
// that exists but belongs to someone else gets the same 404 as one that does not.
func (s *Server) ownedDevice(w http.ResponseWriter, r *http.Request, userID string) (string, bool) {
	deviceID := r.PathValue("id")
	if !uuidPattern.MatchString(deviceID) {
		writeError(w, http.StatusNotFound, "device not found")
		return "", false
	}
	if err := s.store.DeviceOwnedBy(r.Context(), deviceID, userID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "device not found")
		} else {
			serverError(w, "device ownership", err)
		}
		return "", false
	}
	return deviceID, true
}

func (s *Server) getGroup(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUser(w, r)
	if !ok {
		return
	}
	groupID := r.PathValue("id")
	if !uuidPattern.MatchString(groupID) {
		writeError(w, http.StatusNotFound, notFoundGroup)
		return
	}
	g, err := s.store.GetGroup(r.Context(), groupID, userID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, notFoundGroup)
		return
	}
	if err != nil {
		serverError(w, "get group", err)
		return
	}
	writeJSON(w, http.StatusOK, g)
}

func (s *Server) deviceTelemetry(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUser(w, r)
	if !ok {
		return
	}
	name, cr, ok := parseRange(w, r)
	if !ok {
		return
	}
	deviceID, ok := s.ownedDevice(w, r, userID)
	if !ok {
		return
	}
	points, err := s.store.Telemetry(r.Context(), deviceID, time.Now().Add(-cr.window), cr.bucket)
	if err != nil {
		serverError(w, "telemetry", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"range":          name,
		"bucket_seconds": int(cr.bucket.Seconds()),
		"points":         points,
	})
}

// deviceBounds returns the predicted safe zone from the start of the window onwards,
// so the chart gets both the past hours it overlaps and the forecast still ahead.
func (s *Server) deviceBounds(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUser(w, r)
	if !ok {
		return
	}
	name, cr, ok := parseRange(w, r)
	if !ok {
		return
	}
	deviceID, ok := s.ownedDevice(w, r, userID)
	if !ok {
		return
	}
	from := time.Now().Add(-cr.window).Truncate(time.Hour)
	bounds, err := s.store.Bounds(r.Context(), deviceID, from)
	if err != nil {
		serverError(w, "bounds", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"range": name, "bounds": bounds})
}

func (s *Server) dashboardSummary(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUser(w, r)
	if !ok {
		return
	}
	sum, err := s.store.Summary(r.Context(), userID)
	if err != nil {
		serverError(w, "summary", err)
		return
	}
	writeJSON(w, http.StatusOK, sum)
}
