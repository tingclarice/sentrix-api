package ingest

import (
	"encoding/json"
	"fmt"
	"math"
	"net"
	"regexp"
	"strings"
)

// The IoT team's MQTT messages.
//
//	Handshake  topic sentrix/auth/{mac}/connect   (once per connect, from every node)
//	Telemetry  topic sentrix/dt/{meshId}/batch    (every minute, from the mesh root)
//
// MACs arrive as AA:BB:CC:DD:EE:00; the database stores AABBCCDDEEFF.
const (
	handshakeFilter = "sentrix/auth/+/connect"
	batchFilter     = "sentrix/dt/+/batch"
)

var (
	macPattern = regexp.MustCompile(`^[0-9A-F]{12}$`)
	pinPattern = regexp.MustCompile(`^[0-9]{6}$`)
)

// normalizeMAC turns AA:BB:CC:DD:EE:00 (or aa-bb-..., or already AABBCCDDEE00) into
// the database form: 12 uppercase hex characters.
func normalizeMAC(s string) (string, bool) {
	s = strings.ToUpper(strings.NewReplacer(":", "", "-", "").Replace(strings.TrimSpace(s)))
	return s, macPattern.MatchString(s)
}

// topicPart returns the segment between prefix and suffix, for topics like
// sentrix/dt/{meshId}/batch. ok is false if the topic does not have that shape.
func topicPart(topic, prefix, suffix string) (string, bool) {
	if !strings.HasPrefix(topic, prefix) || !strings.HasSuffix(topic, suffix) || len(topic) <= len(prefix)+len(suffix) {
		return "", false
	}
	part := topic[len(prefix) : len(topic)-len(suffix)]
	return part, part != "" && !strings.Contains(part, "/")
}

// Reading is one node's measurement from a batch, ready to store.
type Reading struct {
	MAC                string
	RMSCurrent         float64
	TS                 int64    // Unix seconds, taken from the batch
	CurrentTemperature *float64 // optional
	TargetTemperature  *float64 // optional
}

// Batch is a validated sentrix/dt/{meshId}/batch message.
type Batch struct {
	MeshID       string
	RootMAC      string
	TS           int64
	Readings     []Reading
	OfflineNodes []string // normalized MACs of nodes that did not answer this minute
}

type batchJSON struct {
	MeshID       string   `json:"mesh_id"`
	Timestamp    int64    `json:"timestamp"`
	RootMAC      string   `json:"root_mac"`
	OfflineNodes []string `json:"offline_nodes"`
	Readings     []struct {
		MAC                string   `json:"mac"`
		NodeType           string   `json:"node_type"`
		RMSCurrent         *float64 `json:"rms_current"` // pointer: a missing value must not become 0 A
		CurrentTemperature *float64 `json:"current_temperature"`
		TargetTemperature  *float64 `json:"target_temperature"`
	} `json:"readings"`
}

// parseBatch validates a telemetry batch. err means the whole message is unusable
// (bad JSON, wrong topic, missing mesh_id or timestamp). Individual bad readings and
// offline entries are skipped and described in rejected, so the rest still count.
func parseBatch(topic string, payload []byte) (b Batch, rejected []string, err error) {
	topicMesh, ok := topicPart(topic, "sentrix/dt/", "/batch")
	if !ok {
		return b, nil, fmt.Errorf("unexpected topic %q", topic)
	}
	var j batchJSON
	if err := json.Unmarshal(payload, &j); err != nil {
		return b, nil, err
	}
	if j.MeshID == "" {
		return b, nil, fmt.Errorf("missing mesh_id")
	}
	if j.MeshID != topicMesh {
		return b, nil, fmt.Errorf("payload mesh_id %q does not match topic mesh id %q", j.MeshID, topicMesh)
	}
	if j.Timestamp <= 0 {
		return b, nil, fmt.Errorf("missing or invalid timestamp")
	}

	b = Batch{MeshID: j.MeshID, TS: j.Timestamp}
	if j.RootMAC != "" {
		if mac, ok := normalizeMAC(j.RootMAC); ok {
			b.RootMAC = mac
		} else {
			rejected = append(rejected, fmt.Sprintf("invalid root_mac %q", j.RootMAC))
		}
	}
	seen := map[string]bool{}
	for _, r := range j.Readings {
		mac, ok := normalizeMAC(r.MAC)
		switch {
		case !ok:
			rejected = append(rejected, fmt.Sprintf("invalid mac %q", r.MAC))
		case r.RMSCurrent == nil:
			rejected = append(rejected, fmt.Sprintf("%s: missing rms_current", mac))
		case *r.RMSCurrent < 0 || math.IsNaN(*r.RMSCurrent) || math.IsInf(*r.RMSCurrent, 0):
			rejected = append(rejected, fmt.Sprintf("%s: invalid rms_current %v", mac, *r.RMSCurrent))
		case seen[mac]:
			rejected = append(rejected, fmt.Sprintf("%s: listed twice in one batch", mac))
		default:
			seen[mac] = true
			b.Readings = append(b.Readings, Reading{
				MAC: mac, RMSCurrent: *r.RMSCurrent, TS: j.Timestamp,
				CurrentTemperature: finite(r.CurrentTemperature), TargetTemperature: finite(r.TargetTemperature),
			})
		}
	}
	for _, raw := range j.OfflineNodes {
		mac, ok := normalizeMAC(raw)
		switch {
		case !ok:
			rejected = append(rejected, fmt.Sprintf("invalid offline node %q", raw))
		case seen[mac]:
			rejected = append(rejected, fmt.Sprintf("%s: both reporting and offline", mac))
		default:
			b.OfflineNodes = append(b.OfflineNodes, mac)
		}
	}
	return b, rejected, nil
}

// finite drops NaN/Inf and returns v unchanged otherwise (JSON cannot carry them, but
// a value that overflows float64 parses to Inf).
func finite(v *float64) *float64 {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) {
		return nil
	}
	return v
}

// Handshake is a validated sentrix/auth/{mac}/connect message.
type Handshake struct {
	MAC             string
	PIN             string
	MeshID          string
	Role            string // "root" or "child"
	FirmwareVersion string
	LocalIP         string
	WifiRSSI        *int
	TS              int64
}

type handshakeJSON struct {
	MACAddress      string `json:"mac_address"`
	DevicePIN       string `json:"device_pin"`
	MeshID          string `json:"mesh_id"`
	Role            string `json:"role"`
	FirmwareVersion string `json:"firmware_version"`
	LocalIP         string `json:"local_ip"`
	WifiRSSI        *int   `json:"wifi_rssi"`
	Timestamp       int64  `json:"timestamp"`
}

// parseHandshake validates a handshake. The MAC in the topic and the payload must
// agree, so one node cannot introduce itself as another.
func parseHandshake(topic string, payload []byte) (Handshake, error) {
	topicMAC, ok := topicPart(topic, "sentrix/auth/", "/connect")
	if !ok {
		return Handshake{}, fmt.Errorf("unexpected topic %q", topic)
	}
	var j handshakeJSON
	if err := json.Unmarshal(payload, &j); err != nil {
		return Handshake{}, err
	}
	mac, ok := normalizeMAC(j.MACAddress)
	if !ok {
		return Handshake{}, fmt.Errorf("invalid mac_address %q", j.MACAddress)
	}
	if topicNorm, ok := normalizeMAC(topicMAC); !ok || topicNorm != mac {
		return Handshake{}, fmt.Errorf("payload mac %s does not match topic mac %q", mac, topicMAC)
	}
	if !pinPattern.MatchString(j.DevicePIN) {
		return Handshake{}, fmt.Errorf("device_pin must be 6 digits")
	}
	h := Handshake{MAC: mac, PIN: j.DevicePIN, MeshID: j.MeshID, TS: j.Timestamp}

	switch j.Role {
	case "root", "child":
		h.Role = j.Role
	case "":
	default:
		return Handshake{}, fmt.Errorf("role must be root or child, got %q", j.Role)
	}
	if len(j.FirmwareVersion) > 64 {
		return Handshake{}, fmt.Errorf("firmware_version is too long")
	}
	h.FirmwareVersion = j.FirmwareVersion
	if j.LocalIP != "" {
		if net.ParseIP(j.LocalIP) == nil {
			return Handshake{}, fmt.Errorf("invalid local_ip %q", j.LocalIP)
		}
		h.LocalIP = j.LocalIP
	}
	if j.WifiRSSI != nil && *j.WifiRSSI >= -127 && *j.WifiRSSI <= 0 {
		h.WifiRSSI = j.WifiRSSI // an out-of-range value is dropped, not fatal
	}
	return h, nil
}
