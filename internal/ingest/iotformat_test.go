package ingest

import (
	"os"
	"regexp"
	"testing"
)

// docs/iotformat.md is the IoT team's own description of their messages. This test
// feeds the topics and JSON examples in that file, unedited, through the real parsers,
// so the code and the spec cannot drift apart unnoticed.
func TestIoTTeamSpecExamplesParse(t *testing.T) {
	raw, err := os.ReadFile("../../docs/iotformat.md")
	if err != nil {
		t.Skipf("spec not found: %v", err)
	}
	// Fenced blocks in order: handshake topic, handshake payload, batch topic, batch payload.
	blocks := regexp.MustCompile("(?s)```[a-z]*\n(.*?)```").FindAllSubmatch(raw, -1)
	if len(blocks) != 4 {
		t.Fatalf("found %d code blocks in iotformat.md, want 4 (topic+payload for each message)", len(blocks))
	}
	text := func(i int) string { return string(regexp.MustCompile(`^\s+|\s+$`).ReplaceAll(blocks[i][1], nil)) }

	handshakeTopic, handshakePayload := text(0), text(1)
	batchTopic, batchPayload := text(2), text(3)

	h, err := parseHandshake(handshakeTopic, []byte(handshakePayload))
	if err != nil {
		t.Fatalf("handshake from the spec rejected: %v\ntopic %q\npayload %s", err, handshakeTopic, handshakePayload)
	}
	if h.MAC != "AABBCCDDEE00" || h.PIN != "849201" || h.MeshID != "MESH_89AB" || h.Role != "root" ||
		h.FirmwareVersion != "1.2.4-stable" || h.LocalIP != "192.168.1.45" || h.WifiRSSI == nil || *h.WifiRSSI != -62 {
		t.Errorf("handshake = %+v", h)
	}

	b, rejected, err := parseBatch(batchTopic, []byte(batchPayload))
	if err != nil {
		t.Fatalf("batch from the spec rejected: %v\ntopic %q", err, batchTopic)
	}
	if len(rejected) != 0 {
		t.Errorf("parts of the spec's batch were skipped: %v", rejected)
	}
	if b.MeshID != "MESH_89AB" || b.TS != 1729584060 || b.RootMAC != "AABBCCDDEE00" {
		t.Errorf("batch header = %+v", b)
	}

	want := map[string]struct{ amps, cur, target float64 }{
		"AABBCCDDEE00": {45.25, 38.5, 45},
		"AABBCCDDEE01": {18.5, 32.1, 45},
		"AABBCCDDEE02": {6.15, 29.8, 45},
		"AABBCCDDEE04": {0, 27.5, 45},
	}
	if len(b.Readings) != len(want) {
		t.Fatalf("%d readings, want %d: %+v", len(b.Readings), len(want), b.Readings)
	}
	for _, r := range b.Readings {
		w, ok := want[r.MAC]
		if !ok {
			t.Errorf("unexpected reading for %s", r.MAC)
			continue
		}
		if r.RMSCurrent != w.amps || r.CurrentTemperature == nil || *r.CurrentTemperature != w.cur ||
			r.TargetTemperature == nil || *r.TargetTemperature != w.target || r.TS != 1729584060 {
			t.Errorf("%s = %+v, want %+v", r.MAC, r, w)
		}
	}
	if len(b.OfflineNodes) != 1 || b.OfflineNodes[0] != "AABBCCDDEE03" {
		t.Errorf("offline_nodes = %v, want [AABBCCDDEE03]", b.OfflineNodes)
	}
}
