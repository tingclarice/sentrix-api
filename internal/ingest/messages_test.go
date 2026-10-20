package ingest

import (
	"strings"
	"testing"
)

func TestNormalizeMAC(t *testing.T) {
	for in, want := range map[string]string{
		"AA:BB:CC:DD:EE:00":    "AABBCCDDEE00",
		"aa:bb:cc:dd:ee:00":    "AABBCCDDEE00",
		"aa-bb-cc-dd-ee-00":    "AABBCCDDEE00",
		"AABBCCDDEE00":         "AABBCCDDEE00",
		"  AA:BB:CC:DD:EE:00 ": "AABBCCDDEE00",
	} {
		if got, ok := normalizeMAC(in); !ok || got != want {
			t.Errorf("normalizeMAC(%q) = %q, %v, want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "AA:BB:CC:DD:EE", "AA:BB:CC:DD:EE:00:11", "GG:BB:CC:DD:EE:00", "AABBCCDDEE0"} {
		if _, ok := normalizeMAC(bad); ok {
			t.Errorf("normalizeMAC(%q) accepted", bad)
		}
	}
}

// The batch example the IoT team sent, trimmed to the fields Go uses.
const teamBatch = `{
  "mesh_id": "MESH_89AB", "timestamp": 1729584060, "root_mac": "AA:BB:CC:DD:EE:00",
  "readings": [
    {"mac":"AA:BB:CC:DD:EE:00","node_type":"root","rms_current":45.25,"current_temperature":38.5,"target_temperature":45.0},
    {"mac":"AA:BB:CC:DD:EE:01","node_type":"child","rms_current":18.50,"current_temperature":32.1,"target_temperature":45.0},
    {"mac":"AA:BB:CC:DD:EE:04","node_type":"child","rms_current":0.00,"current_temperature":27.5,"target_temperature":45.0}
  ],
  "offline_nodes": ["AA:BB:CC:DD:EE:03"]
}`

const batchTopic = "sentrix/dt/MESH_89AB/batch"

func TestParseBatchTeamExample(t *testing.T) {
	b, rejected, err := parseBatch(batchTopic, []byte(teamBatch))
	if err != nil || len(rejected) != 0 {
		t.Fatalf("err %v, rejected %v", err, rejected)
	}
	if b.MeshID != "MESH_89AB" || b.TS != 1729584060 || b.RootMAC != "AABBCCDDEE00" {
		t.Errorf("batch = %+v", b)
	}
	if len(b.Readings) != 3 || b.Readings[1].MAC != "AABBCCDDEE01" || b.Readings[1].RMSCurrent != 18.5 {
		t.Fatalf("readings = %+v", b.Readings)
	}
	// Every reading takes the batch's timestamp.
	for _, r := range b.Readings {
		if r.TS != 1729584060 {
			t.Errorf("%s ts = %d, want the batch timestamp", r.MAC, r.TS)
		}
	}
	// 0.00 A is a real reading (a stopped machine), not a missing one.
	if last := b.Readings[2]; last.RMSCurrent != 0 {
		t.Errorf("idle node = %+v", last)
	}
	if r := b.Readings[0]; r.CurrentTemperature == nil || *r.CurrentTemperature != 38.5 || *r.TargetTemperature != 45 {
		t.Errorf("temperatures = %v / %v", r.CurrentTemperature, r.TargetTemperature)
	}
	if len(b.OfflineNodes) != 1 || b.OfflineNodes[0] != "AABBCCDDEE03" {
		t.Errorf("offline = %v", b.OfflineNodes)
	}
}

func TestParseBatchRejectsTheWholeMessage(t *testing.T) {
	for name, tt := range map[string]struct{ topic, payload string }{
		"not json":                {batchTopic, `nope`},
		"wrong topic":             {"sentrix/dt/MESH_89AB/other", teamBatch},
		"no mesh id":              {batchTopic, `{"timestamp":1729584060,"readings":[]}`},
		"mesh differs from topic": {"sentrix/dt/OTHER_MESH/batch", teamBatch},
		"no timestamp":            {batchTopic, `{"mesh_id":"MESH_89AB","readings":[]}`},
		"zero timestamp":          {batchTopic, `{"mesh_id":"MESH_89AB","timestamp":0,"readings":[]}`},
		"empty mesh in topic":     {"sentrix/dt//batch", `{"mesh_id":"","timestamp":1}`},
	} {
		if _, _, err := parseBatch(tt.topic, []byte(tt.payload)); err == nil {
			t.Errorf("%s: accepted, want an error", name)
		}
	}
}

func TestParseBatchSkipsBadReadingsButKeepsTheRest(t *testing.T) {
	payload := `{"mesh_id":"M","timestamp":1729584060,
	  "readings":[
	    {"mac":"not-a-mac","rms_current":1},
	    {"mac":"AA:BB:CC:DD:EE:01"},
	    {"mac":"AA:BB:CC:DD:EE:02","rms_current":-3},
	    {"mac":"AA:BB:CC:DD:EE:03","rms_current":7},
	    {"mac":"aabbccddee03","rms_current":8},
	    {"mac":"AA:BB:CC:DD:EE:05","rms_current":2}],
	  "offline_nodes":["AA:BB:CC:DD:EE:03","zz","AA:BB:CC:DD:EE:09"]}`
	b, rejected, err := parseBatch("sentrix/dt/M/batch", []byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Readings) != 2 || b.Readings[0].MAC != "AABBCCDDEE03" || b.Readings[0].RMSCurrent != 7 || b.Readings[1].MAC != "AABBCCDDEE05" {
		t.Errorf("readings = %+v, want only .03 (first copy) and .05", b.Readings)
	}
	for _, want := range []string{"invalid mac", "missing rms_current", "invalid rms_current", "listed twice", "invalid offline node", "both reporting and offline"} {
		found := false
		for _, r := range rejected {
			found = found || strings.Contains(r, want)
		}
		if !found {
			t.Errorf("rejected %v is missing %q", rejected, want)
		}
	}
	// .03 reports and is also listed offline: the reading wins, only .09 is offline.
	if len(b.OfflineNodes) != 1 || b.OfflineNodes[0] != "AABBCCDDEE09" {
		t.Errorf("offline = %v, want only AABBCCDDEE09", b.OfflineNodes)
	}
}

func TestParseBatchWithoutTemperatures(t *testing.T) {
	b, _, err := parseBatch("sentrix/dt/M/batch",
		[]byte(`{"mesh_id":"M","timestamp":5,"readings":[{"mac":"AA:BB:CC:DD:EE:01","rms_current":4}]}`))
	if err != nil || len(b.Readings) != 1 {
		t.Fatalf("%v %+v", err, b)
	}
	if r := b.Readings[0]; r.CurrentTemperature != nil || r.TargetTemperature != nil {
		t.Errorf("missing temperatures must stay nil, got %v / %v", r.CurrentTemperature, r.TargetTemperature)
	}
	if len(b.OfflineNodes) != 0 {
		t.Errorf("offline = %v", b.OfflineNodes)
	}
}

const teamHandshake = `{
  "mac_address": "AA:BB:CC:DD:EE:00", "device_pin": "849201", "mesh_id": "MESH_89AB",
  "role": "root", "firmware_version": "1.2.4-stable", "local_ip": "192.168.1.45",
  "wifi_rssi": -62, "timestamp": 1792465260
}`

const handshakeTopic = "sentrix/auth/AA:BB:CC:DD:EE:00/connect"

func TestParseHandshakeTeamExample(t *testing.T) {
	h, err := parseHandshake(handshakeTopic, []byte(teamHandshake))
	if err != nil {
		t.Fatal(err)
	}
	if h.MAC != "AABBCCDDEE00" || h.PIN != "849201" || h.MeshID != "MESH_89AB" || h.Role != "root" ||
		h.FirmwareVersion != "1.2.4-stable" || h.LocalIP != "192.168.1.45" || h.WifiRSSI == nil || *h.WifiRSSI != -62 || h.TS != 1792465260 {
		t.Errorf("handshake = %+v", h)
	}
}

func TestParseHandshakeRejects(t *testing.T) {
	edit := func(old, new string) string { return strings.Replace(teamHandshake, old, new, 1) }
	for name, tt := range map[string]struct{ topic, payload string }{
		"not json":               {handshakeTopic, `nope`},
		"wrong topic":            {"sentrix/auth/AA:BB:CC:DD:EE:00/other", teamHandshake},
		"mac differs from topic": {"sentrix/auth/AA:BB:CC:DD:EE:99/connect", teamHandshake},
		"invalid mac":            {handshakeTopic, edit(`"AA:BB:CC:DD:EE:00", "device_pin"`, `"nope", "device_pin"`)},
		"pin too short":          {handshakeTopic, edit("849201", "8492")},
		"pin not digits":         {handshakeTopic, edit("849201", "84920a")},
		"pin as a number":        {handshakeTopic, edit(`"849201"`, `849201`)},
		"unknown role":           {handshakeTopic, edit(`"root"`, `"boss"`)},
		"bad ip":                 {handshakeTopic, edit("192.168.1.45", "999.1.1.1")},
		"huge firmware string":   {handshakeTopic, edit("1.2.4-stable", strings.Repeat("x", 65))},
	} {
		if _, err := parseHandshake(tt.topic, []byte(tt.payload)); err == nil {
			t.Errorf("%s: accepted, want an error", name)
		}
	}
}

func TestParseHandshakeOptionalAndOutOfRangeFields(t *testing.T) {
	h, err := parseHandshake("sentrix/auth/AABBCCDDEE00/connect",
		[]byte(`{"mac_address":"aabbccddee00","device_pin":"000123","mesh_id":"M","wifi_rssi":500}`))
	if err != nil {
		t.Fatal(err)
	}
	if h.PIN != "000123" { // leading zeros survive
		t.Errorf("pin = %q", h.PIN)
	}
	if h.WifiRSSI != nil {
		t.Errorf("an impossible rssi (500) should be dropped, got %d", *h.WifiRSSI)
	}
	if h.Role != "" || h.LocalIP != "" || h.FirmwareVersion != "" {
		t.Errorf("absent fields should stay empty: %+v", h)
	}
}
