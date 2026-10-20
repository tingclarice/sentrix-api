package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/tingclarice/sentrix-api/internal/store"
)

const (
	maxBodyBytes  = 1 << 20
	maxNameLen    = 100
	maxDescLen    = 500
	userMaxFails  = 5  // failed claims allowed per user per 15 minutes
	macMaxFails   = 20 // ...and per device MAC across all users, which stops guessing from many accounts
	notFoundGroup = "group not found"
)

var (
	macPattern = regexp.MustCompile(`^[0-9A-F]{12}$`)
	pinPattern = regexp.MustCompile(`^[0-9]{6}$`)
)

// decodeJSON reads a small JSON body, rejecting oversized or malformed input.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

// requireUser returns the authenticated user ID; RequireAuth has already set it.
func requireUser(w http.ResponseWriter, r *http.Request) (string, bool) {
	id, ok := UserID(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "not authenticated")
	}
	return id, ok
}

// ownedGroup checks that the {id} group belongs to the user. A group that exists but
// belongs to someone else gets the same 404 as one that does not exist.
func (s *Server) ownedGroup(w http.ResponseWriter, r *http.Request, userID string) (string, bool) {
	groupID := r.PathValue("id")
	if !uuidPattern.MatchString(groupID) {
		writeError(w, http.StatusNotFound, notFoundGroup)
		return "", false
	}
	if err := s.store.GroupOwnedBy(r.Context(), groupID, userID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, notFoundGroup)
		} else {
			serverError(w, "group ownership", err)
		}
		return "", false
	}
	return groupID, true
}

func serverError(w http.ResponseWriter, what string, err error) {
	log.Printf("%s: %v", what, err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

func (s *Server) createGroup(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUser(w, r)
	if !ok {
		return
	}
	var req struct {
		Name        string  `json:"name"`
		Description *string `json:"description"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || utf8.RuneCountInString(name) > maxNameLen {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("name is required (max %d characters)", maxNameLen))
		return
	}
	if req.Description != nil && utf8.RuneCountInString(*req.Description) > maxDescLen {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("description is too long (max %d characters)", maxDescLen))
		return
	}

	g, err := s.store.CreateGroup(r.Context(), userID, name, req.Description)
	if err != nil {
		serverError(w, "create group", err)
		return
	}
	writeJSON(w, http.StatusCreated, g)
}

func (s *Server) listGroups(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUser(w, r)
	if !ok {
		return
	}
	groups, err := s.store.ListGroups(r.Context(), userID)
	if err != nil {
		serverError(w, "list groups", err)
		return
	}
	writeJSON(w, http.StatusOK, groups)
}

func (s *Server) listDevices(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUser(w, r)
	if !ok {
		return
	}
	groupID, ok := s.ownedGroup(w, r, userID)
	if !ok {
		return
	}
	devices, err := s.store.ListDevices(r.Context(), groupID)
	if err != nil {
		serverError(w, "list devices", err)
		return
	}
	writeJSON(w, http.StatusOK, devices)
}

// claimDevice attaches a factory-seeded device to the user's group. The device is
// identified by the MAC on its sticker and proven by its 6-digit PIN. Wrong guesses
// are rate limited per user and per MAC, and every failure looks the same ("no
// match") so the response does not reveal which MACs exist or are already claimed.
func (s *Server) claimDevice(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUser(w, r)
	if !ok {
		return
	}
	groupID, ok := s.ownedGroup(w, r, userID)
	if !ok {
		return
	}

	var req struct {
		MAC  string  `json:"mac"`
		PIN  string  `json:"pin"`
		Name *string `json:"name"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	// The sticker and the firmware write AA:BB:CC:DD:EE:FF; the database stores AABBCCDDEEFF.
	mac := strings.ToUpper(strings.NewReplacer(":", "", "-", "").Replace(strings.TrimSpace(req.MAC)))
	if !macPattern.MatchString(mac) {
		writeError(w, http.StatusBadRequest, "mac must be 12 hex characters, e.g. AABBCCDDEEFF or AA:BB:CC:DD:EE:FF")
		return
	}
	if !pinPattern.MatchString(req.PIN) {
		writeError(w, http.StatusBadRequest, "pin must be 6 digits")
		return
	}
	var name *string
	if req.Name != nil {
		trimmed := strings.TrimSpace(*req.Name)
		if utf8.RuneCountInString(trimmed) > maxNameLen {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("name is too long (max %d characters)", maxNameLen))
			return
		}
		if trimmed != "" {
			name = &trimmed
		}
	}

	userKey, macKey := "user:"+userID, "mac:"+mac
	for _, l := range []struct {
		limiter *failureLimiter
		key     string
	}{{s.userLimiter, userKey}, {s.macLimiter, macKey}} {
		if allowed, wait := l.limiter.allow(l.key); !allowed {
			w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
			writeError(w, http.StatusTooManyRequests, "too many failed attempts, try again later")
			return
		}
	}

	d, err := s.store.ClaimDevice(r.Context(), groupID, mac, req.PIN, name, s.calibrationMinutes)
	if errors.Is(err, store.ErrNotFound) {
		s.userLimiter.fail(userKey)
		s.macLimiter.fail(macKey)
		writeError(w, http.StatusNotFound, "no unclaimed device matches that MAC and PIN")
		return
	}
	if err != nil {
		serverError(w, "claim device", err)
		return
	}
	if s.onDeviceChanged != nil {
		s.onDeviceChanged(mac)
	}
	writeJSON(w, http.StatusOK, d)
}
