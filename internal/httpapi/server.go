// Package httpapi serves the REST API used by the Next.js dashboard.
package httpapi

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"slices"
	"time"

	"github.com/tingclarice/sentrix-api/internal/store"
)

// Pinger is satisfied by *pgxpool.Pool; /health uses it to check the database.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Deps are the things the Server needs; main wires the real ones, tests use fakes.
type Deps struct {
	DB                 Pinger
	Store              store.Store
	Auth               *Verifier
	AllowedOrigins     []string
	CalibrationMinutes int
	// OnDeviceChanged is called with a device's MAC after the API changes its row, so
	// the MQTT side can drop any cached copy.
	OnDeviceChanged func(mac string)
}

type Server struct {
	db                 Pinger
	store              store.Store
	auth               *Verifier
	origins            []string
	calibrationMinutes int
	onDeviceChanged    func(mac string)
	userLimiter        *failureLimiter
	macLimiter         *failureLimiter
}

func New(d Deps) *Server {
	return &Server{
		db:                 d.DB,
		store:              d.Store,
		auth:               d.Auth,
		origins:            d.AllowedOrigins,
		calibrationMinutes: d.CalibrationMinutes,
		onDeviceChanged:    d.OnDeviceChanged,
		userLimiter:        newFailureLimiter(userMaxFails, 15*time.Minute),
		macLimiter:         newFailureLimiter(macMaxFails, 15*time.Minute),
	}
}

// Handler returns the router wrapped in recovery, logging and CORS.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", s.health)
	authed := func(pattern string, h http.HandlerFunc) {
		mux.Handle(pattern, s.auth.RequireAuth(h))
	}
	authed("GET /api/me", s.me)
	authed("POST /api/groups", s.createGroup)
	authed("GET /api/groups", s.listGroups)
	authed("POST /api/groups/{id}/devices", s.claimDevice)
	authed("GET /api/groups/{id}", s.getGroup)
	authed("GET /api/groups/{id}/devices", s.listDevices)
	authed("GET /api/devices/{id}/telemetry", s.deviceTelemetry)
	authed("GET /api/devices/{id}/bounds", s.deviceBounds)
	authed("GET /api/dashboard/summary", s.dashboardSummary)
	authed("GET /api/alerts", s.listAlerts)
	authed("PATCH /api/alerts/{id}/feedback", s.alertFeedback)
	authed("PATCH /api/alerts/{id}/resolve", s.resolveAlert)
	authed("POST /api/push/subscribe", s.pushSubscribe)
	authed("DELETE /api/push/subscribe", s.pushUnsubscribe)

	return s.recoverer(s.logger(s.cors(mux)))
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.db.Ping(ctx); err != nil {
		log.Printf("health: database ping: %v", err)
		writeError(w, http.StatusServiceUnavailable, "database unreachable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// me echoes the authenticated user; it lets the frontend confirm a token works.
func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	id, _ := UserID(r.Context())
	writeJSON(w, http.StatusOK, map[string]string{"user_id": id})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write response: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// cors allows the configured browser origins to call the API with an
// Authorization header, and answers preflight requests.
func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && slices.Contains(s.origins, origin) {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Add("Vary", "Origin")
			h.Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			h.Set("Access-Control-Max-Age", "600")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (s *Server) logger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, rec.status, time.Since(start).Round(time.Millisecond))
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				log.Printf("panic serving %s %s: %v", r.Method, r.URL.Path, v)
				writeError(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
