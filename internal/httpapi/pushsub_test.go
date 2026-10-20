package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/tingclarice/sentrix-api/internal/pushtest"
	"github.com/tingclarice/sentrix-api/internal/store"
)

const goodEndpoint = "https://fcm.googleapis.com/fcm/send/abc123"

// browserKeys returns the p256dh and auth values a real browser would send.
func browserKeys(t *testing.T) (p256dh, auth string) {
	t.Helper()
	b := pushtest.NewBrowser(t)
	return b.P256dh, b.Auth
}

func subscriptionJSON(endpoint, p256dh, auth string) string {
	b, _ := json.Marshal(map[string]any{
		"endpoint": endpoint, "expirationTime": nil,
		"keys": map[string]string{"p256dh": p256dh, "auth": auth},
	})
	return string(b)
}

func TestPushEndpointsNeedAuth(t *testing.T) {
	a := newAPITest(t)
	for _, method := range []string{"POST", "DELETE"} {
		if got := a.as(t, "", method, "/api/push/subscribe", "{}").Code; got != http.StatusUnauthorized {
			t.Errorf("%s without a token: %d, want 401", method, got)
		}
	}
}

func TestPushSubscribe(t *testing.T) {
	a := newAPITest(t)
	p256dh, auth := browserKeys(t)

	rec := a.as(t, "alice", "POST", "/api/push/subscribe", subscriptionJSON(goodEndpoint, p256dh, auth))
	if rec.Code != http.StatusCreated {
		t.Fatalf("subscribe: %d %s", rec.Code, rec.Body)
	}
	row, ok := a.store.pushSubs[goodEndpoint]
	if !ok || row.userID != "alice" || row.sub.P256dh != p256dh || row.sub.Auth != auth {
		t.Errorf("stored = %+v", a.store.pushSubs)
	}

	// Subscribing again (a page reload) is fine and does not duplicate.
	if got := a.as(t, "alice", "POST", "/api/push/subscribe", subscriptionJSON(goodEndpoint, p256dh, auth)).Code; got != http.StatusCreated {
		t.Errorf("re-subscribe: %d, want 201", got)
	}
	if len(a.store.pushSubs) != 1 {
		t.Errorf("%d rows after subscribing twice, want 1", len(a.store.pushSubs))
	}

	// Padded base64 from a client is stored without the padding.
	other := "https://updates.push.services.mozilla.com/wpush/v2/xyz"
	a.as(t, "alice", "POST", "/api/push/subscribe", subscriptionJSON(other, p256dh+"==", auth+"=="))
	if a.store.pushSubs[other].sub.Auth != auth {
		t.Errorf("auth stored as %q, want %q", a.store.pushSubs[other].sub.Auth, auth)
	}
}

func TestPushSubscribeRejectsBadInput(t *testing.T) {
	a := newAPITest(t)
	p256dh, auth := browserKeys(t)
	short := base64.RawURLEncoding.EncodeToString([]byte("too short"))

	for name, body := range map[string]string{
		"not json":             `nope`,
		"empty":                `{}`,
		"http endpoint":        subscriptionJSON("http://fcm.googleapis.com/fcm/send/abc", p256dh, auth),
		"internal host (SSRF)": subscriptionJSON("https://169.254.169.254/latest/meta-data", p256dh, auth),
		"localhost":            subscriptionJSON("https://localhost/push", p256dh, auth),
		"lookalike host":       subscriptionJSON("https://fcm.googleapis.com.evil.example/x", p256dh, auth),
		"suffix without a dot": subscriptionJSON("https://evilpush.apple.com.example/x", p256dh, auth),
		"credentials in url":   subscriptionJSON("https://user:pw@fcm.googleapis.com/x", p256dh, auth),
		"odd port":             subscriptionJSON("https://fcm.googleapis.com:8443/x", p256dh, auth),
		"no endpoint":          subscriptionJSON("", p256dh, auth),
		"huge endpoint":        subscriptionJSON(goodEndpoint+string(make([]byte, 0))+fmt.Sprintf("%02048d", 0), p256dh, auth),
		"short p256dh":         subscriptionJSON(goodEndpoint, short, auth),
		"short auth":           subscriptionJSON(goodEndpoint, p256dh, short),
		"keys not base64":      subscriptionJSON(goodEndpoint, "!!!", "???"),
		"missing keys":         subscriptionJSON(goodEndpoint, "", ""),
	} {
		if got := a.as(t, "alice", "POST", "/api/push/subscribe", body).Code; got != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", name, got)
		}
	}
	if len(a.store.pushSubs) != 0 {
		t.Errorf("rejected requests stored %d rows", len(a.store.pushSubs))
	}
}

func TestPushHostAllowList(t *testing.T) {
	for host, want := range map[string]bool{
		"fcm.googleapis.com":                  true,
		"updates.push.services.mozilla.com":   true,
		"web.push.apple.com":                  true,
		"wns2-par02p.notify.windows.com":      true,
		"FCM.GOOGLEAPIS.COM":                  true,
		"googleapis.com":                      false,
		"evil.googleapis.com":                 false,
		"push.apple.com":                      false, // only subdomains match a leading-dot suffix
		"notify.windows.com.attacker.example": false,
		"127.0.0.1":                           false,
		"":                                    false,
	} {
		if got := pushHostAllowed(host); got != want {
			t.Errorf("pushHostAllowed(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestPushSubscriptionLimit(t *testing.T) {
	a := newAPITest(t)
	p256dh, auth := browserKeys(t)
	for i := 0; i < store.MaxPushSubscriptionsPerUser; i++ {
		ep := fmt.Sprintf("https://fcm.googleapis.com/fcm/send/%d", i)
		if got := a.as(t, "alice", "POST", "/api/push/subscribe", subscriptionJSON(ep, p256dh, auth)).Code; got != http.StatusCreated {
			t.Fatalf("subscription %d: %d, want 201", i, got)
		}
	}
	over := subscriptionJSON("https://fcm.googleapis.com/fcm/send/one-too-many", p256dh, auth)
	if got := a.as(t, "alice", "POST", "/api/push/subscribe", over).Code; got != http.StatusConflict {
		t.Errorf("over the limit: %d, want 409", got)
	}
	if got := a.as(t, "bob", "POST", "/api/push/subscribe", over).Code; got != http.StatusCreated {
		t.Errorf("another user: %d, want 201 (the limit is per user)", got)
	}
}

func TestPushUnsubscribe(t *testing.T) {
	a := newAPITest(t)
	p256dh, auth := browserKeys(t)
	a.as(t, "alice", "POST", "/api/push/subscribe", subscriptionJSON(goodEndpoint, p256dh, auth))
	body := fmt.Sprintf(`{"endpoint":%q}`, goodEndpoint)

	// Someone else cannot remove it, and gets no hint whether it exists.
	if got := a.as(t, "bob", "DELETE", "/api/push/subscribe", body).Code; got != http.StatusNoContent {
		t.Errorf("bob: %d, want 204", got)
	}
	if len(a.store.pushSubs) != 1 {
		t.Fatal("bob removed alice's subscription")
	}

	if got := a.as(t, "alice", "DELETE", "/api/push/subscribe", body).Code; got != http.StatusNoContent {
		t.Errorf("alice: %d, want 204", got)
	}
	if len(a.store.pushSubs) != 0 {
		t.Error("subscription still stored")
	}
	// Unsubscribing twice is harmless.
	if got := a.as(t, "alice", "DELETE", "/api/push/subscribe", body).Code; got != http.StatusNoContent {
		t.Errorf("second delete: %d, want 204", got)
	}
	for _, bad := range []string{`{}`, `nope`} {
		if got := a.as(t, "alice", "DELETE", "/api/push/subscribe", bad).Code; got != http.StatusBadRequest {
			t.Errorf("delete %q: %d, want 400", bad, got)
		}
	}
}
