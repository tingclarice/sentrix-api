package ingest

import (
	"encoding/json"
	"fmt"
	"strings"
)

// parseTelemetry turns an MQTT telemetry message into valid readings.
// Readings that fail validation are skipped and described in rejected, so the
// caller can log them. err is set only when the payload is not valid JSON.
//
// The topic is sentrix/telemetry/{mac}. A single reading must carry that same
// MAC. Batches come from the mesh root and carry leaf MACs, so they are exempt.
func parseTelemetry(topic string, payload []byte) (readings []Reading, rejected []string, err error) {
	var p Payload
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil, nil, err
	}
	all := p.Readings
	batched := len(all) > 0
	if !batched {
		all = []Reading{p.Reading}
	}

	topicMAC := strings.ToUpper(topic[strings.LastIndex(topic, "/")+1:])
	for _, r := range all {
		r.MAC = strings.ToUpper(r.MAC)
		switch {
		case !macPattern.MatchString(r.MAC):
			rejected = append(rejected, fmt.Sprintf("invalid mac %q", r.MAC))
		case !batched && r.MAC != topicMAC:
			rejected = append(rejected, fmt.Sprintf("payload mac %s does not match topic mac %s", r.MAC, topicMAC))
		case r.TS <= 0:
			rejected = append(rejected, fmt.Sprintf("%s: missing or invalid ts", r.MAC))
		default:
			readings = append(readings, r)
		}
	}
	return readings, rejected, nil
}
