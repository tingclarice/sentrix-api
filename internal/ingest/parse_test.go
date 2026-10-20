package ingest

import (
	"strings"
	"testing"
)

func TestParseTelemetry(t *testing.T) {
	const topic = "sentrix/telemetry/AABBCCDDEEFF"

	tests := []struct {
		name         string
		topic        string
		payload      string
		wantMACs     []string
		wantRejected []string // substrings
		wantErr      bool
	}{
		{
			name:     "single reading",
			topic:    topic,
			payload:  `{"mac":"AABBCCDDEEFF","rms_current":5.2,"ts":1760421000}`,
			wantMACs: []string{"AABBCCDDEEFF"},
		},
		{
			name:     "lowercase mac is normalised",
			topic:    topic,
			payload:  `{"mac":"aabbccddeeff","rms_current":5.2,"ts":1760421000}`,
			wantMACs: []string{"AABBCCDDEEFF"},
		},
		{
			name:         "payload mac differs from topic mac",
			topic:        topic,
			payload:      `{"mac":"112233445566","rms_current":5.2,"ts":1760421000}`,
			wantRejected: []string{"does not match topic"},
		},
		{
			name:         "mac with colons is invalid",
			topic:        topic,
			payload:      `{"mac":"AA:BB:CC:DD:EE:FF","rms_current":5.2,"ts":1760421000}`,
			wantRejected: []string{"invalid mac"},
		},
		{
			name:         "missing ts",
			topic:        topic,
			payload:      `{"mac":"AABBCCDDEEFF","rms_current":5.2}`,
			wantRejected: []string{"missing or invalid ts"},
		},
		{
			name:  "batch from the root carries leaf macs",
			topic: topic,
			payload: `{"readings":[
				{"mac":"AABBCCDDEEFF","rms_current":5.2,"ts":1760421000},
				{"mac":"112233445566","rms_current":0.4,"ts":1760421000}]}`,
			wantMACs: []string{"AABBCCDDEEFF", "112233445566"},
		},
		{
			name:  "one bad reading in a batch does not drop the rest",
			topic: topic,
			payload: `{"readings":[
				{"mac":"AABBCCDDEEFF","rms_current":5.2,"ts":0},
				{"mac":"112233445566","rms_current":0.4,"ts":1760421000}]}`,
			wantMACs:     []string{"112233445566"},
			wantRejected: []string{"missing or invalid ts"},
		},
		{
			name:    "not json",
			topic:   topic,
			payload: `not json`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, rejected, err := parseTelemetry(tt.topic, []byte(tt.payload))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if len(got) != len(tt.wantMACs) {
				t.Fatalf("got %d readings, want %d: %+v", len(got), len(tt.wantMACs), got)
			}
			for i, mac := range tt.wantMACs {
				if got[i].MAC != mac {
					t.Errorf("reading %d mac = %s, want %s", i, got[i].MAC, mac)
				}
			}
			if len(rejected) != len(tt.wantRejected) {
				t.Fatalf("rejected = %v, want %d entries", rejected, len(tt.wantRejected))
			}
			for i, sub := range tt.wantRejected {
				if !strings.Contains(rejected[i], sub) {
					t.Errorf("rejected[%d] = %q, want it to contain %q", i, rejected[i], sub)
				}
			}
		})
	}
}
