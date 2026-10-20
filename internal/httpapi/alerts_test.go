package httpapi

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tingclarice/sentrix-api/internal/store"
)

const (
	alertAlice = "aaaa1111-0000-0000-0000-000000000001"
	alertBob   = "bbbb1111-0000-0000-0000-000000000001"
)

func newAlertTest(t *testing.T) *apiTest {
	a := newAPITest(t)
	a.store.alerts = []store.Alert{
		{ID: alertAlice, DeviceID: devAlice, DeviceMAC: "AABBCCDDEEFF", Severity: "CRITICAL", ActualValue: 15},
		{ID: alertBob, DeviceID: devBob, DeviceMAC: "112233445566", Severity: "WARNING"},
	}
	a.store.alertOwners = map[string]string{alertAlice: "alice", alertBob: "bob"}
	return a
}

func TestAlertRoutesNeedAuth(t *testing.T) {
	a := newAlertTest(t)
	for _, route := range []struct{ method, path string }{
		{"GET", "/api/alerts"},
		{"PATCH", "/api/alerts/" + alertAlice + "/feedback"},
		{"PATCH", "/api/alerts/" + alertAlice + "/resolve"},
	} {
		if got := a.as(t, "", route.method, route.path, `{"user_feedback":true}`).Code; got != http.StatusUnauthorized {
			t.Errorf("%s %s without a token: %d, want 401", route.method, route.path, got)
		}
	}
}

func TestListAlertsOnlyShowsTheUsersOwn(t *testing.T) {
	a := newAlertTest(t)
	rec := a.as(t, "alice", "GET", "/api/alerts", "")
	var got []store.Alert
	json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != http.StatusOK || len(got) != 1 || got[0].ID != alertAlice {
		t.Errorf("alice: %d %s", rec.Code, rec.Body)
	}
	if rec := a.as(t, "carol", "GET", "/api/alerts", ""); strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Errorf("a user with no alerts gets %s, want []", rec.Body)
	}
}

func TestListAlertsFilters(t *testing.T) {
	a := newAlertTest(t)

	a.as(t, "alice", "GET", "/api/alerts", "")
	if f := a.store.lastAlertFilter; f.Limit != defaultAlertLimit || f.Status != "" || f.Reviewed != nil || f.Before != nil {
		t.Errorf("defaults = %+v", f)
	}

	q := url.Values{
		"status": {"all"}, "group": {gAlice}, "device": {devAlice}, "reviewed": {"false"},
		"limit": {"10"}, "before": {"2026-10-20T08:00:00Z"},
	}
	if rec := a.as(t, "alice", "GET", "/api/alerts?"+q.Encode(), ""); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	f := a.store.lastAlertFilter
	if f.Status != "all" || f.GroupID != gAlice || f.DeviceID != devAlice || f.Limit != 10 ||
		f.Reviewed == nil || *f.Reviewed || f.Before == nil || !f.Before.Equal(time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)) {
		t.Errorf("filter = %+v", f)
	}
}

func TestListAlertsRejectsBadFilters(t *testing.T) {
	a := newAlertTest(t)
	for _, q := range []string{
		"status=open", "limit=0", "limit=201", "limit=abc", "reviewed=maybe",
		"before=yesterday", "group=nope", "device=nope",
	} {
		if got := a.as(t, "alice", "GET", "/api/alerts?"+q, "").Code; got != http.StatusBadRequest {
			t.Errorf("?%s: %d, want 400", q, got)
		}
	}
}

func TestAlertFeedback(t *testing.T) {
	a := newAlertTest(t)
	path := "/api/alerts/" + alertAlice + "/feedback"

	for _, want := range []bool{true, false} {
		body := `{"user_feedback":true}`
		if !want {
			body = `{"user_feedback":false}`
		}
		rec := a.as(t, "alice", "PATCH", path, body)
		var got store.Alert
		json.Unmarshal(rec.Body.Bytes(), &got)
		if rec.Code != http.StatusOK || got.UserFeedback == nil || *got.UserFeedback != want {
			t.Errorf("feedback %v: %d %s", want, rec.Code, rec.Body)
		}
	}

	// false must be accepted as a value, but a missing or null field is not.
	for _, body := range []string{`{}`, `{"user_feedback":null}`, `{"user_feedback":"yes"}`, `nope`} {
		if got := a.as(t, "alice", "PATCH", path, body).Code; got != http.StatusBadRequest {
			t.Errorf("feedback body %q: %d, want 400", body, got)
		}
	}
}

func TestResolveAlert(t *testing.T) {
	a := newAlertTest(t)
	rec := a.as(t, "alice", "PATCH", "/api/alerts/"+alertAlice+"/resolve", "")
	var got store.Alert
	json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != http.StatusOK || !got.IsResolved {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
}

func TestOtherUsersAlertIsNotFound(t *testing.T) {
	a := newAlertTest(t)
	for _, id := range []string{alertBob, "cccc1111-0000-0000-0000-000000000001", "not-a-uuid"} {
		if got := a.as(t, "alice", "PATCH", "/api/alerts/"+id+"/resolve", "").Code; got != http.StatusNotFound {
			t.Errorf("resolve %q: %d, want 404", id, got)
		}
		if got := a.as(t, "alice", "PATCH", "/api/alerts/"+id+"/feedback", `{"user_feedback":true}`).Code; got != http.StatusNotFound {
			t.Errorf("feedback %q: %d, want 404", id, got)
		}
	}
}
