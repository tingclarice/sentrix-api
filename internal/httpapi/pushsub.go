package httpapi

import (
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/tingclarice/sentrix-api/internal/store"
)

const maxEndpointLen = 2048

// pushHostSuffixes are the push services browsers use. The server later POSTs to the
// endpoint a client registers, so accepting any URL would let a user make the server
// call arbitrary hosts (SSRF). A suffix starting with "." matches subdomains only.
var pushHostSuffixes = []string{
	"fcm.googleapis.com",         // Chrome, Edge, Brave, Samsung Internet, Android browsers
	".push.services.mozilla.com", // Firefox
	".push.apple.com",            // Safari
	".notify.windows.com",        // Windows Notification Service
}

func pushHostAllowed(host string) bool {
	host = strings.ToLower(host)
	for _, suffix := range pushHostSuffixes {
		if host == suffix || (strings.HasPrefix(suffix, ".") && strings.HasSuffix(host, suffix)) {
			return true
		}
	}
	return false
}

// validEndpoint accepts only https URLs on the standard port, without credentials,
// whose host is a known push service.
func validEndpoint(raw string) bool {
	if raw == "" || len(raw) > maxEndpointLen {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Hostname() == "" {
		return false
	}
	if port := u.Port(); port != "" && port != "443" {
		return false
	}
	return pushHostAllowed(u.Hostname())
}

// decodeKey decodes a base64url value (padding optional) and checks its length.
func decodeKey(v string, wantLen int) bool {
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(v, "="))
	return err == nil && len(b) == wantLen
}

// pushSubscription is the JSON a browser's PushSubscription.toJSON() produces.
type pushSubscription struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

func (s *Server) pushSubscribe(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUser(w, r)
	if !ok {
		return
	}
	var req pushSubscription
	if !decodeJSON(w, r, &req) {
		return
	}
	if !validEndpoint(req.Endpoint) {
		writeError(w, http.StatusBadRequest, "endpoint must be an https URL of a browser push service")
		return
	}
	// 65 bytes: an uncompressed P-256 public key. 16 bytes: the auth secret.
	if !decodeKey(req.Keys.P256dh, 65) || !decodeKey(req.Keys.Auth, 16) {
		writeError(w, http.StatusBadRequest, "keys.p256dh and keys.auth must be valid base64url push keys")
		return
	}
	err := s.store.SavePushSubscription(r.Context(), userID, store.PushSubscription{
		Endpoint: req.Endpoint,
		P256dh:   strings.TrimRight(req.Keys.P256dh, "="),
		Auth:     strings.TrimRight(req.Keys.Auth, "="),
	})
	if errors.Is(err, store.ErrTooManySubscriptions) {
		writeError(w, http.StatusConflict, "too many push subscriptions for this account")
		return
	}
	if err != nil {
		serverError(w, "save push subscription", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"status": "subscribed"})
}

func (s *Server) pushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUser(w, r)
	if !ok {
		return
	}
	var req struct {
		Endpoint string `json:"endpoint"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Endpoint == "" || len(req.Endpoint) > maxEndpointLen {
		writeError(w, http.StatusBadRequest, "endpoint is required")
		return
	}
	if err := s.store.DeletePushSubscription(r.Context(), userID, req.Endpoint); err != nil {
		serverError(w, "delete push subscription", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
