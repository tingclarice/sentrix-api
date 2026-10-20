package push

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/SherClockHolmes/webpush-go"

	"github.com/tingclarice/sentrix-api/internal/pushtest"
	"github.com/tingclarice/sentrix-api/internal/store"
)

type fakeSubs struct {
	mu      sync.Mutex
	subs    []store.PushSubscription
	loadErr error
	deleted []string
}

func (f *fakeSubs) PushSubscriptionsForDevice(context.Context, string) ([]store.PushSubscription, error) {
	return f.subs, f.loadErr
}

func (f *fakeSubs) DeletePushSubscriptionByEndpoint(_ context.Context, endpoint string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, endpoint)
	return nil
}

type received struct {
	path, authorization, urgency, ttl, encoding string
	body                                        []byte
}

// pushService is a fake browser push service: it records each request and answers
// with the status registered for its path (201 by default).
type pushService struct {
	*httptest.Server
	mu       sync.Mutex
	requests []received
	status   map[string]int
	browsers map[string]*pushtest.Browser // the subscriber behind each path
}

func newPushService() *pushService {
	ps := &pushService{status: map[string]int{}, browsers: map[string]*pushtest.Browser{}}
	ps.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ps.mu.Lock()
		defer ps.mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		ps.requests = append(ps.requests, received{
			path: r.URL.Path, authorization: r.Header.Get("Authorization"),
			urgency: r.Header.Get("Urgency"), ttl: r.Header.Get("TTL"),
			encoding: r.Header.Get("Content-Encoding"), body: body,
		})
		code := ps.status[r.URL.Path]
		if code == 0 {
			code = http.StatusCreated
		}
		w.WriteHeader(code)
	}))
	return ps
}

func newTestSender(t *testing.T, subs *fakeSubs, ps *pushService) *Sender {
	t.Helper()
	priv, pub, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		t.Fatal(err)
	}
	s := New(subs, pub, priv, "mailto:ops@example.com")
	s.client = ps.Client()
	return s
}

func sub(t *testing.T, ps *pushService, path string) store.PushSubscription {
	t.Helper()
	b := pushtest.NewBrowser(t)
	ps.browsers[path] = b
	return store.PushSubscription{Endpoint: ps.URL + path, P256dh: b.P256dh, Auth: b.Auth}
}

var testAlert = Alert{
	ID: "alert-1", DeviceID: "dev-1", DeviceName: "Pump 1", DeviceMAC: "AABBCCDDEEFF",
	Severity: "CRITICAL", Value: 15, Min: 4, Max: 7,
}

func TestBuildPayload(t *testing.T) {
	raw, err := buildPayload(testAlert)
	if err != nil {
		t.Fatal(err)
	}
	var p payload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	if p.Title != "CRITICAL alert: Pump 1" || p.Body != "15.00A, expected 4.00-7.00A" ||
		p.AlertID != "alert-1" || p.DeviceID != "dev-1" || p.Severity != "CRITICAL" || p.URL != "/alerts" {
		t.Errorf("payload = %+v", p)
	}

	unnamed := testAlert
	unnamed.DeviceName = ""
	raw, _ = buildPayload(unnamed)
	if !strings.Contains(string(raw), "CRITICAL alert: AABBCCDDEEFF") {
		t.Errorf("an unnamed device should fall back to its MAC: %s", raw)
	}
}

func TestSendDeliversEncryptedPushWithVAPID(t *testing.T) {
	ps := newPushService()
	defer ps.Close()
	subs := &fakeSubs{subs: []store.PushSubscription{sub(t, ps, "/a"), sub(t, ps, "/b")}}

	if err := newTestSender(t, subs, ps).Send(context.Background(), testAlert); err != nil {
		t.Fatal(err)
	}
	if len(ps.requests) != 2 {
		t.Fatalf("%d pushes sent, want 2", len(ps.requests))
	}
	for _, r := range ps.requests {
		if !strings.HasPrefix(r.authorization, "vapid t=") {
			t.Errorf("%s: Authorization = %q, want a VAPID header", r.path, r.authorization)
		}
		if r.encoding != "aes128gcm" {
			t.Errorf("%s: Content-Encoding = %q, want aes128gcm", r.path, r.encoding)
		}
		// Only the subscriber's own key can read it, and it says what we meant.
		plain, err := ps.browsers[r.path].Decrypt(r.body)
		if err != nil {
			t.Errorf("%s: browser could not decrypt the push: %v", r.path, err)
			continue
		}
		var p payload
		if err := json.Unmarshal(plain, &p); err != nil || p.Title != "CRITICAL alert: Pump 1" || p.AlertID != "alert-1" {
			t.Errorf("%s: decrypted payload = %s (%v)", r.path, plain, err)
		}
		if other := ps.browsers["/b"]; r.path == "/a" {
			if _, err := other.Decrypt(r.body); err == nil {
				t.Error("a different browser could decrypt a push meant for /a")
			}
		}
		if r.ttl != "3600" {
			t.Errorf("%s: TTL = %q, want 3600", r.path, r.ttl)
		}
		if r.urgency != "high" {
			t.Errorf("%s: Urgency = %q, want high for CRITICAL", r.path, r.urgency)
		}
	}
	if len(subs.deleted) != 0 {
		t.Errorf("deleted %v on success", subs.deleted)
	}

	// A WARNING is not urgent.
	ps.requests = nil
	warn := testAlert
	warn.Severity = "WARNING"
	newTestSender(t, subs, ps).Send(context.Background(), warn)
	if ps.requests[0].urgency != "normal" {
		t.Errorf("WARNING urgency = %q, want normal", ps.requests[0].urgency)
	}
}

func TestSendRemovesSubscriptionsThatAreGone(t *testing.T) {
	ps := newPushService()
	defer ps.Close()
	ps.status["/gone"] = http.StatusGone
	ps.status["/missing"] = http.StatusNotFound
	ps.status["/broken"] = http.StatusInternalServerError
	subs := &fakeSubs{subs: []store.PushSubscription{
		sub(t, ps, "/gone"), sub(t, ps, "/missing"), sub(t, ps, "/broken"), sub(t, ps, "/ok"),
	}}

	if err := newTestSender(t, subs, ps).Send(context.Background(), testAlert); err != nil {
		t.Fatal(err)
	}
	if len(ps.requests) != 4 {
		t.Errorf("%d pushes sent, want all 4 attempted despite the failures", len(ps.requests))
	}
	got := strings.Join(subs.deleted, ",")
	if len(subs.deleted) != 2 || !strings.HasSuffix(subs.deleted[0], "/gone") || !strings.HasSuffix(subs.deleted[1], "/missing") {
		t.Errorf("deleted %q, want only the 410 and 404 subscriptions", got)
	}
}

func TestSendSkipsABadSubscriptionAndContinues(t *testing.T) {
	ps := newPushService()
	defer ps.Close()
	good := sub(t, ps, "/ok")
	bad := store.PushSubscription{Endpoint: ps.URL + "/bad", P256dh: "!!not base64!!", Auth: "x"}
	subs := &fakeSubs{subs: []store.PushSubscription{bad, good}}

	if err := newTestSender(t, subs, ps).Send(context.Background(), testAlert); err != nil {
		t.Fatal(err)
	}
	if len(ps.requests) != 1 || ps.requests[0].path != "/ok" {
		t.Errorf("requests = %+v, want only the valid subscription", ps.requests)
	}
}

func TestSendWithNoSubscriptionsOrADatabaseError(t *testing.T) {
	ps := newPushService()
	defer ps.Close()
	s := newTestSender(t, &fakeSubs{}, ps)
	if err := s.Send(context.Background(), testAlert); err != nil || len(ps.requests) != 0 {
		t.Errorf("no subscriptions: err %v, %d requests", err, len(ps.requests))
	}
	s = newTestSender(t, &fakeSubs{loadErr: errors.New("db down")}, ps)
	if err := s.Send(context.Background(), testAlert); err == nil {
		t.Error("expected the database error to be returned")
	}
}
