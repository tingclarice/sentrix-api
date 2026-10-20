package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/tingclarice/sentrix-api/internal/store"
)

// fakeStore is an in-memory store.Store. Groups are owned by group.owner; devices
// are seeded UNCLAIMED with a MAC and PIN.
type fakeStore struct {
	groups  map[string]string // group id -> owner
	devices map[string]*fakeDevice
	nextID  int

	deviceOwners   map[string]string // device id -> owner, for the read endpoints
	lastTelemetry  telemetryCall
	lastBoundsFrom time.Time

	pushSubs        map[string]pushRow // endpoint -> row
	alerts          []store.Alert
	alertOwners     map[string]string // alert id -> owner
	lastAlertFilter store.AlertFilter
	claims          int // ClaimDevice calls, to check the limiter blocks before the store
}

func (f *fakeStore) GetGroup(_ context.Context, groupID, userID string) (store.Group, error) {
	if f.groups[groupID] != userID {
		return store.Group{}, store.ErrNotFound
	}
	return store.Group{ID: groupID, Name: "detail", MeshID: "0123456789AB"}, nil
}

func (f *fakeStore) DeviceOwnedBy(_ context.Context, deviceID, userID string) error {
	if f.deviceOwners[deviceID] != userID {
		return store.ErrNotFound
	}
	return nil
}

func (f *fakeStore) Telemetry(_ context.Context, deviceID string, since time.Time, bucket time.Duration) ([]store.TelemetryPoint, error) {
	f.lastTelemetry = telemetryCall{deviceID, since, bucket}
	return []store.TelemetryPoint{{Timestamp: since, RMSCurrent: 4, MaxRMSCurrent: 9}}, nil
}

func (f *fakeStore) Bounds(_ context.Context, deviceID string, from time.Time) ([]store.Bound, error) {
	f.lastBoundsFrom = from
	return []store.Bound{{Timestamp: from, UpperBound: 7, LowerBound: 3}}, nil
}

func (f *fakeStore) Summary(_ context.Context, userID string) (store.Summary, error) {
	return store.Summary{Groups: 2, Devices: 3, Active: 2, Calibrating: 1}, nil
}

func (f *fakeStore) ListAlerts(_ context.Context, userID string, flt store.AlertFilter) ([]store.Alert, error) {
	f.lastAlertFilter = flt
	out := []store.Alert{}
	for _, a := range f.alerts {
		if f.alertOwners[a.ID] == userID {
			out = append(out, a)
		}
	}
	return out, nil
}

func (f *fakeStore) SetAlertFeedback(_ context.Context, alertID, userID string, real bool) (store.Alert, error) {
	a, ok := f.alertFor(alertID, userID)
	if !ok {
		return store.Alert{}, store.ErrNotFound
	}
	a.UserFeedback = &real
	return a, nil
}

func (f *fakeStore) ResolveAlert(_ context.Context, alertID, userID string) (store.Alert, error) {
	a, ok := f.alertFor(alertID, userID)
	if !ok {
		return store.Alert{}, store.ErrNotFound
	}
	a.IsResolved = true
	return a, nil
}

func (f *fakeStore) alertFor(alertID, userID string) (store.Alert, bool) {
	for _, a := range f.alerts {
		if a.ID == alertID && f.alertOwners[alertID] == userID {
			return a, true
		}
	}
	return store.Alert{}, false
}

func (f *fakeStore) SavePushSubscription(_ context.Context, userID string, sub store.PushSubscription) error {
	if f.pushSubs == nil {
		f.pushSubs = map[string]pushRow{}
	}
	if _, exists := f.pushSubs[sub.Endpoint]; !exists {
		n := 0
		for _, row := range f.pushSubs {
			if row.userID == userID {
				n++
			}
		}
		if n >= store.MaxPushSubscriptionsPerUser {
			return store.ErrTooManySubscriptions
		}
	}
	f.pushSubs[sub.Endpoint] = pushRow{userID, sub}
	return nil
}

func (f *fakeStore) DeletePushSubscription(_ context.Context, userID, endpoint string) error {
	if row, ok := f.pushSubs[endpoint]; ok && row.userID == userID {
		delete(f.pushSubs, endpoint)
	}
	return nil
}

func (f *fakeStore) PushSubscriptionsForDevice(context.Context, string) ([]store.PushSubscription, error) {
	return nil, nil
}

func (f *fakeStore) DeletePushSubscriptionByEndpoint(context.Context, string) error { return nil }

type pushRow struct {
	userID string
	sub    store.PushSubscription
}

type telemetryCall struct {
	deviceID string
	since    time.Time
	bucket   time.Duration
}

type fakeDevice struct {
	store.Device
	pin string
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		groups: map[string]string{gAlice: "alice", gBob: "bob"},
		devices: map[string]*fakeDevice{
			"AABBCCDDEEFF": {Device: store.Device{ID: "d1", MACAddress: "AABBCCDDEEFF", Status: "UNCLAIMED"}, pin: "123456"},
		},
	}
}

func (f *fakeStore) CreateGroup(_ context.Context, userID, name string, desc *string) (store.Group, error) {
	f.nextID++
	id := "g-new-" + string(rune('0'+f.nextID))
	f.groups[id] = userID
	return store.Group{ID: id, Name: name, Description: desc, MeshID: "0123456789AB", CreatedAt: time.Now()}, nil
}

func (f *fakeStore) ListGroups(_ context.Context, userID string) ([]store.Group, error) {
	out := []store.Group{}
	for id, owner := range f.groups {
		if owner == userID {
			out = append(out, store.Group{ID: id, Name: id})
		}
	}
	return out, nil
}

func (f *fakeStore) GroupOwnedBy(_ context.Context, groupID, userID string) error {
	if f.groups[groupID] != userID {
		return store.ErrNotFound
	}
	return nil
}

func (f *fakeStore) ClaimDevice(_ context.Context, groupID, mac, pin string, name *string, minutes int) (store.Device, error) {
	f.claims++
	d, ok := f.devices[mac]
	if !ok || d.pin != pin || d.Status != "UNCLAIMED" {
		return store.Device{}, store.ErrNotFound
	}
	d.Status, d.GroupID, d.Name = "CALIBRATING", &groupID, name
	end := time.Now().Add(time.Duration(minutes) * time.Minute)
	d.CalibrationEndsAt = &end
	return d.Device, nil
}

func (f *fakeStore) ListDevices(_ context.Context, groupID string) ([]store.Device, error) {
	out := []store.Device{}
	for _, d := range f.devices {
		if d.GroupID != nil && *d.GroupID == groupID {
			out = append(out, d.Device)
		}
	}
	return out, nil
}

const (
	gAlice = "aaaaaaaa-0000-0000-0000-000000000001"
	gBob   = "bbbbbbbb-0000-0000-0000-000000000001"
)

type apiTest struct {
	h       http.Handler
	store   *fakeStore
	changed []string
}

func newAPITest(t *testing.T) *apiTest {
	t.Helper()
	a := &apiTest{store: newFakeStore()}
	a.h = New(Deps{
		DB:                 fakeDB{},
		Store:              a.store,
		Auth:               &Verifier{secret: []byte(testSecret)},
		AllowedOrigins:     []string{"http://localhost:3000"},
		CalibrationMinutes: 10080,
		OnDeviceChanged:    func(mac string) { a.changed = append(a.changed, mac) },
	}).Handler()
	return a
}

// as sends a request as the given user (empty = no token).
func (a *apiTest) as(t *testing.T, user, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if user != "" {
		tok := token(t, jwt.SigningMethodHS256, []byte(testSecret), func(c *jwt.RegisteredClaims) { c.Subject = user })
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	rec := httptest.NewRecorder()
	a.h.ServeHTTP(rec, req)
	return rec
}

func TestGroupRoutesNeedAuth(t *testing.T) {
	a := newAPITest(t)
	for _, route := range []struct{ method, path string }{
		{"POST", "/api/groups"}, {"GET", "/api/groups"},
		{"POST", "/api/groups/" + gAlice + "/devices"}, {"GET", "/api/groups/" + gAlice + "/devices"},
	} {
		if got := a.as(t, "", route.method, route.path, "{}").Code; got != http.StatusUnauthorized {
			t.Errorf("%s %s without a token: %d, want 401", route.method, route.path, got)
		}
	}
}

func TestCreateAndListGroups(t *testing.T) {
	a := newAPITest(t)

	rec := a.as(t, "alice", "POST", "/api/groups", `{"name":"  Building A  ","description":"north wing"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var g store.Group
	json.Unmarshal(rec.Body.Bytes(), &g)
	if g.Name != "Building A" || g.MeshID == "" {
		t.Errorf("group = %+v, want trimmed name and a mesh id", g)
	}

	for _, body := range []string{`{}`, `{"name":"   "}`, `not json`, `{"name":"` + strings.Repeat("x", 101) + `"}`} {
		if got := a.as(t, "alice", "POST", "/api/groups", body).Code; got != http.StatusBadRequest {
			t.Errorf("create with %.20q: %d, want 400", body, got)
		}
	}

	rec = a.as(t, "alice", "GET", "/api/groups", "")
	var list []store.Group
	json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list) != 2 { // seeded gAlice + the one just created
		t.Errorf("alice sees %d groups, want 2: %s", len(list), rec.Body)
	}
	rec = a.as(t, "carol", "GET", "/api/groups", "")
	if strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Errorf("a user with no groups gets %s, want []", rec.Body)
	}
}

func TestClaimDevice(t *testing.T) {
	a := newAPITest(t)
	claim := `{"mac":"aabbccddeeff","pin":"123456","name":"Pump 1"}`

	rec := a.as(t, "alice", "POST", "/api/groups/"+gAlice+"/devices", claim)
	if rec.Code != http.StatusOK {
		t.Fatalf("claim: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "123456") || strings.Contains(rec.Body.String(), "pin") {
		t.Errorf("response leaks the PIN: %s", rec.Body)
	}
	var d store.Device
	json.Unmarshal(rec.Body.Bytes(), &d)
	if d.Status != "CALIBRATING" || d.CalibrationEndsAt == nil {
		t.Errorf("device = %+v, want CALIBRATING with an end time", d)
	}
	if want := time.Now().Add(10080 * time.Minute); d.CalibrationEndsAt.Sub(want) > time.Minute {
		t.Errorf("calibration ends %v, want about %v", d.CalibrationEndsAt, want)
	}
	if len(a.changed) != 1 || a.changed[0] != "AABBCCDDEEFF" {
		t.Errorf("OnDeviceChanged calls = %v, want [AABBCCDDEEFF]", a.changed)
	}

	// Already claimed: same answer as a wrong PIN.
	if got := a.as(t, "alice", "POST", "/api/groups/"+gAlice+"/devices", claim).Code; got != http.StatusNotFound {
		t.Errorf("claiming twice: %d, want 404", got)
	}

	rec = a.as(t, "alice", "GET", "/api/groups/"+gAlice+"/devices", "")
	var list []store.Device
	json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list) != 1 || list[0].MACAddress != "AABBCCDDEEFF" {
		t.Errorf("device list = %s", rec.Body)
	}
}

func TestClaimValidation(t *testing.T) {
	a := newAPITest(t)
	for _, body := range []string{
		`{"mac":"AA:BB:CC:DD:EE","pin":"123456"}`,
		`{"mac":"AABBCCDDEEFFAA","pin":"123456"}`,
		`{"mac":"AABBCCDDEEFF","pin":"12345"}`,
		`{"mac":"AABBCCDDEEFF","pin":"abcdef"}`,
		`{"mac":"AABBCCDDEEFF"}`,
		`nope`,
	} {
		if got := a.as(t, "alice", "POST", "/api/groups/"+gAlice+"/devices", body).Code; got != http.StatusBadRequest {
			t.Errorf("claim with %q: %d, want 400", body, got)
		}
	}
	if a.store.claims != 0 {
		t.Errorf("invalid requests reached the store %d times", a.store.claims)
	}
}

func TestOtherUsersGroupIsNotFound(t *testing.T) {
	a := newAPITest(t)
	// bob's group, alice's token: same 404 as a group that does not exist.
	for _, path := range []string{"/api/groups/" + gBob + "/devices", "/api/groups/nope/devices"} {
		if got := a.as(t, "alice", "GET", path, "").Code; got != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 404", path, got)
		}
		if got := a.as(t, "alice", "POST", path, `{"mac":"AABBCCDDEEFF","pin":"123456"}`).Code; got != http.StatusNotFound {
			t.Errorf("POST %s: %d, want 404", path, got)
		}
	}
	if a.store.claims != 0 || len(a.changed) != 0 {
		t.Error("a device was claimed into another user's group")
	}
}

func TestPINBruteForceIsLimitedPerUser(t *testing.T) {
	a := newAPITest(t)
	wrong := `{"mac":"AABBCCDDEEFF","pin":"000000"}`
	for i := 0; i < userMaxFails; i++ {
		if got := a.as(t, "alice", "POST", "/api/groups/"+gAlice+"/devices", wrong).Code; got != http.StatusNotFound {
			t.Fatalf("attempt %d: %d, want 404", i+1, got)
		}
	}

	// The next attempt is blocked, even with the right PIN, and never reaches the store.
	before := a.store.claims
	rec := a.as(t, "alice", "POST", "/api/groups/"+gAlice+"/devices", `{"mac":"AABBCCDDEEFF","pin":"123456"}`)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Errorf("after %d failures: %d (Retry-After %q), want 429 with Retry-After", userMaxFails, rec.Code, rec.Header().Get("Retry-After"))
	}
	if a.store.claims != before {
		t.Error("a blocked attempt reached the store")
	}

	// One user's failures do not lock the real owner out of the device.
	if got := a.as(t, "bob", "POST", "/api/groups/"+gBob+"/devices", `{"mac":"AABBCCDDEEFF","pin":"123456"}`).Code; got != http.StatusOK {
		t.Errorf("another user with the right PIN: %d, want 200", got)
	}
}

func TestPINBruteForceIsLimitedPerMAC(t *testing.T) {
	a := newAPITest(t)
	wrong := `{"mac":"AABBCCDDEEFF","pin":"000000"}`
	// Many accounts each stay under their own limit but share the MAC's budget.
	for i := 0; i < macMaxFails; i++ {
		user := "attacker-" + string(rune('a'+i/userMaxFails))
		group := fmt.Sprintf("cccccccc-0000-0000-0000-%012d", i/userMaxFails)
		a.store.groups[group] = user
		a.as(t, user, "POST", "/api/groups/"+group+"/devices", wrong)
	}
	rec := a.as(t, "bob", "POST", "/api/groups/"+gBob+"/devices", `{"mac":"AABBCCDDEEFF","pin":"123456"}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("after %d failures on one MAC from several users: %d, want 429", macMaxFails, rec.Code)
	}
}

func TestFailureLimiterWindow(t *testing.T) {
	clock := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	l := newFailureLimiter(2, time.Minute)
	l.now = func() time.Time { return clock }

	l.fail("k")
	l.fail("k")
	if ok, wait := l.allow("k"); ok || wait <= 0 || wait > time.Minute {
		t.Errorf("after max failures: allowed=%v wait=%v, want blocked with a wait up to 1m", ok, wait)
	}
	if ok, _ := l.allow("other"); !ok {
		t.Error("an unrelated key must not be blocked")
	}
	clock = clock.Add(61 * time.Second)
	if ok, _ := l.allow("k"); !ok {
		t.Error("failures older than the window must expire")
	}
}

func TestClaimAcceptsAMACWrittenWithColons(t *testing.T) {
	a := newAPITest(t)
	for _, mac := range []string{"AA:BB:CC:DD:EE:FF", "aa-bb-cc-dd-ee-ff"} {
		a = newAPITest(t) // a fresh unclaimed device each time
		body := `{"mac":"` + mac + `","pin":"123456"}`
		if got := a.as(t, "alice", "POST", "/api/groups/"+gAlice+"/devices", body).Code; got != http.StatusOK {
			t.Errorf("claim with mac %q: %d, want 200", mac, got)
		}
	}
}
