package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/tingclarice/sentrix-api/internal/store"
)

const (
	defaultAlertLimit = 50
	maxAlertLimit     = 200
)

// listAlerts is the alert feed. Filters: status=unresolved|resolved|all (default
// unresolved), group, device, reviewed=true|false, limit (default 50, max 200) and
// before=<RFC3339 timestamp> to page backwards from the last alert received.
func (s *Server) listAlerts(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUser(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	f := store.AlertFilter{Status: q.Get("status"), GroupID: q.Get("group"), DeviceID: q.Get("device"), Limit: defaultAlertLimit}

	switch f.Status {
	case "", "unresolved", "resolved", "all":
	default:
		writeError(w, http.StatusBadRequest, "status must be unresolved, resolved or all")
		return
	}
	for name, id := range map[string]string{"group": f.GroupID, "device": f.DeviceID} {
		if id != "" && !uuidPattern.MatchString(id) {
			writeError(w, http.StatusBadRequest, name+" must be a UUID")
			return
		}
	}
	if v := q.Get("reviewed"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "reviewed must be true or false")
			return
		}
		f.Reviewed = &b
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxAlertLimit {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and "+strconv.Itoa(maxAlertLimit))
			return
		}
		f.Limit = n
	}
	if v := q.Get("before"); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "before must be an RFC 3339 timestamp")
			return
		}
		f.Before = &t
	}

	alerts, err := s.store.ListAlerts(r.Context(), userID, f)
	if err != nil {
		serverError(w, "list alerts", err)
		return
	}
	writeJSON(w, http.StatusOK, alerts)
}

// alertID returns the {id} path value, answering 404 for anything that is not a UUID.
func alertID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !uuidPattern.MatchString(id) {
		writeError(w, http.StatusNotFound, "alert not found")
		return "", false
	}
	return id, true
}

func alertResult(w http.ResponseWriter, a store.Alert, err error, what string) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "alert not found")
		return
	}
	if err != nil {
		serverError(w, what, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// alertFeedback records whether the user judged the alert a real anomaly (true) or a
// false alarm (false). It is stored for later model tuning.
func (s *Server) alertFeedback(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUser(w, r)
	if !ok {
		return
	}
	id, ok := alertID(w, r)
	if !ok {
		return
	}
	var req struct {
		UserFeedback *bool `json:"user_feedback"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.UserFeedback == nil {
		writeError(w, http.StatusBadRequest, "user_feedback must be true or false")
		return
	}
	a, err := s.store.SetAlertFeedback(r.Context(), id, userID, *req.UserFeedback)
	alertResult(w, a, err, "alert feedback")
}

func (s *Server) resolveAlert(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUser(w, r)
	if !ok {
		return
	}
	id, ok := alertID(w, r)
	if !ok {
		return
	}
	a, err := s.store.ResolveAlert(r.Context(), id, userID)
	alertResult(w, a, err, "resolve alert")
}
