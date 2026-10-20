package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

const (
	devAlice = "11111111-0000-0000-0000-000000000001"
	devBob   = "22222222-0000-0000-0000-000000000001"
)

func newReadTest(t *testing.T) *apiTest {
	a := newAPITest(t)
	a.store.deviceOwners = map[string]string{devAlice: "alice", devBob: "bob"}
	return a
}

func TestReadRoutesNeedAuth(t *testing.T) {
	a := newReadTest(t)
	for _, path := range []string{
		"/api/groups/" + gAlice,
		"/api/devices/" + devAlice + "/telemetry",
		"/api/devices/" + devAlice + "/bounds",
		"/api/dashboard/summary",
	} {
		if got := a.as(t, "", "GET", path, "").Code; got != http.StatusUnauthorized {
			t.Errorf("GET %s without a token: %d, want 401", path, got)
		}
	}
}

func TestTelemetryRanges(t *testing.T) {
	a := newReadTest(t)
	tests := []struct {
		query      string
		wantRange  string
		wantBucket time.Duration
		wantWindow time.Duration
	}{
		{"", "24h", 5 * time.Minute, 24 * time.Hour}, // default
		{"?range=1h", "1h", 0, time.Hour},
		{"?range=24h", "24h", 5 * time.Minute, 24 * time.Hour},
		{"?range=7d", "7d", 15 * time.Minute, 7 * 24 * time.Hour},
	}
	for _, tt := range tests {
		rec := a.as(t, "alice", "GET", "/api/devices/"+devAlice+"/telemetry"+tt.query, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%q: %d %s", tt.query, rec.Code, rec.Body)
		}
		var body struct {
			Range         string `json:"range"`
			BucketSeconds int    `json:"bucket_seconds"`
			Points        []struct {
				RMS float64 `json:"rms_current"`
				Max float64 `json:"max_rms_current"`
			} `json:"points"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Range != tt.wantRange || body.BucketSeconds != int(tt.wantBucket.Seconds()) {
			t.Errorf("%q: range %q bucket %ds, want %q %v", tt.query, body.Range, body.BucketSeconds, tt.wantRange, tt.wantBucket)
		}
		if len(body.Points) != 1 || body.Points[0].Max != 9 {
			t.Errorf("%q: points = %+v", tt.query, body.Points)
		}
		got := a.store.lastTelemetry
		if got.bucket != tt.wantBucket || got.deviceID != devAlice {
			t.Errorf("%q: store called with %+v", tt.query, got)
		}
		if age := time.Since(got.since); age < tt.wantWindow-time.Minute || age > tt.wantWindow+time.Minute {
			t.Errorf("%q: window starts %v ago, want about %v", tt.query, age, tt.wantWindow)
		}
	}
}

func TestBoundsStartAtTheWindowAndKeepTheForecast(t *testing.T) {
	a := newReadTest(t)
	rec := a.as(t, "alice", "GET", "/api/devices/"+devAlice+"/bounds?range=24h", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	from := a.store.lastBoundsFrom
	if from.Minute() != 0 || from.Second() != 0 {
		t.Errorf("bounds start %v, want the top of an hour", from)
	}
	if age := time.Since(from); age < 24*time.Hour || age > 25*time.Hour+time.Minute {
		t.Errorf("bounds start %v ago, want 24-25h", age)
	}
	if !strings.Contains(rec.Body.String(), `"upper_bound":7`) {
		t.Errorf("body = %s", rec.Body)
	}
}

func TestBadRangeIs400(t *testing.T) {
	a := newReadTest(t)
	for _, path := range []string{"telemetry", "bounds"} {
		for _, q := range []string{"?range=30d", "?range=", "?range=24H"} {
			want := http.StatusBadRequest
			if q == "?range=" { // empty means "use the default"
				want = http.StatusOK
			}
			if got := a.as(t, "alice", "GET", "/api/devices/"+devAlice+"/"+path+q, "").Code; got != want {
				t.Errorf("%s%s: %d, want %d", path, q, got, want)
			}
		}
	}
}

func TestOtherUsersDeviceIsNotFound(t *testing.T) {
	a := newReadTest(t)
	for _, id := range []string{devBob, "33333333-0000-0000-0000-000000000001", "not-a-uuid"} {
		for _, path := range []string{"telemetry", "bounds"} {
			if got := a.as(t, "alice", "GET", "/api/devices/"+id+"/"+path, "").Code; got != http.StatusNotFound {
				t.Errorf("GET device %q %s: %d, want 404", id, path, got)
			}
		}
	}
	if a.store.lastTelemetry.deviceID != "" {
		t.Error("telemetry was queried for a device the user does not own")
	}
}

func TestGroupDetail(t *testing.T) {
	a := newReadTest(t)
	if got := a.as(t, "alice", "GET", "/api/groups/"+gAlice, "").Code; got != http.StatusOK {
		t.Errorf("own group: %d, want 200", got)
	}
	for _, id := range []string{gBob, "nope", "dddddddd-0000-0000-0000-000000000001"} {
		if got := a.as(t, "alice", "GET", "/api/groups/"+id, "").Code; got != http.StatusNotFound {
			t.Errorf("group %q: %d, want 404", id, got)
		}
	}
}

func TestSummary(t *testing.T) {
	a := newReadTest(t)
	rec := a.as(t, "alice", "GET", "/api/dashboard/summary", "")
	var sum map[string]int
	json.Unmarshal(rec.Body.Bytes(), &sum)
	if rec.Code != http.StatusOK || sum["devices"] != 3 || sum["active"] != 2 {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
	for _, key := range []string{"groups", "calibrating", "offline", "anomalies_24h", "unresolved_alerts"} {
		if _, ok := sum[key]; !ok {
			t.Errorf("summary is missing %q: %s", key, rec.Body)
		}
	}
}
