// Command simulator publishes fake ESP32 telemetry to the MQTT broker.
//
// It can backfill past days (to give the ML side something to train on),
// then keep publishing live readings, optionally injecting current spikes.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"math/rand"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// colonMAC formats AABBCCDDEEFF as AA:BB:CC:DD:EE:FF, the way the firmware sends it.
// Input that is not 12 characters is returned unchanged.
func colonMAC(mac string) string {
	mac = strings.ToUpper(strings.NewReplacer(":", "", "-", "").Replace(mac))
	if len(mac) != 12 {
		return mac
	}
	parts := make([]string, 6)
	for i := range parts {
		parts[i] = mac[2*i : 2*i+2]
	}
	return strings.Join(parts, ":")
}

type batchReading struct {
	MAC                string  `json:"mac"`
	NodeType           string  `json:"node_type"`
	RMSCurrent         float64 `json:"rms_current"`
	CurrentTemperature float64 `json:"current_temperature"`
	TargetTemperature  float64 `json:"target_temperature"`
}

// batch is sentrix/dt/{mesh_id}/batch, as the IoT team defined it.
type batch struct {
	MeshID       string         `json:"mesh_id"`
	Timestamp    int64          `json:"timestamp"`
	RootMAC      string         `json:"root_mac"`
	Readings     []batchReading `json:"readings"`
	OfflineNodes []string       `json:"offline_nodes"`
}

// handshake is sentrix/auth/{mac}/connect.
type handshake struct {
	MACAddress      string `json:"mac_address"`
	DevicePIN       string `json:"device_pin"`
	MeshID          string `json:"mesh_id"`
	Role            string `json:"role"`
	FirmwareVersion string `json:"firmware_version"`
	LocalIP         string `json:"local_ip"`
	WifiRSSI        int    `json:"wifi_rssi"`
	Timestamp       int64  `json:"timestamp"`
}

func main() {
	broker := flag.String("broker", envOr("MQTT_BROKER_URL", "tcp://localhost:1883"), "MQTT broker URL")
	user := flag.String("user", os.Getenv("MQTT_USERNAME"), "MQTT username")
	pass := flag.String("pass", os.Getenv("MQTT_PASSWORD"), "MQTT password")
	mac := flag.String("mac", "AABBCCDDEEFF", "device MAC, with or without colons")
	pin := flag.String("pin", "123456", "device PIN, sent in the handshake")
	mesh := flag.String("mesh", "MESH_89AB", "mesh_id; must match the device's group once it has one")
	noHandshake := flag.Bool("no-handshake", false, "skip the handshake message")
	backfill := flag.Int("backfill-days", 0, "publish this many past days of readings first (1 per minute)")
	delay := flag.Duration("backfill-delay", 150*time.Millisecond, "pause between backfill messages; the Go service ingests ~8-9/s, and the broker drops messages once its per-client queue (default 1000) overflows")
	live := flag.Bool("live", false, "after any backfill, keep publishing a reading every -interval")
	interval := flag.Duration("interval", time.Minute, "delay between live readings (use e.g. 1s to speed up)")
	spikeEvery := flag.Int("spike-every", 0, "in live mode, inject a spike every N readings (0 = never)")
	spikeAmps := flag.Float64("spike-amps", 15, "current in amps for an injected spike")
	seed := flag.Int64("seed", 1, "random seed (same seed gives the same noise)")
	flag.Parse()

	if *backfill == 0 && !*live {
		log.Fatal("nothing to do: pass -backfill-days N and/or -live")
	}

	rng := rand.New(rand.NewSource(*seed))
	opts := mqtt.NewClientOptions().
		AddBroker(*broker).
		SetUsername(*user).
		SetPassword(*pass).
		SetClientID(fmt.Sprintf("sentrix-simulator-%s", *mac))
	client := mqtt.NewClient(opts)
	if t := client.Connect(); t.Wait() && t.Error() != nil {
		log.Fatal(t.Error())
	}
	defer client.Disconnect(250)

	nodeMAC := colonMAC(*mac)
	send := func(topic string, v any) error {
		b, _ := json.Marshal(v)
		t := client.Publish(topic, 1, false, b)
		t.Wait()
		return t.Error()
	}
	if !*noHandshake {
		err := send("sentrix/auth/"+nodeMAC+"/connect", handshake{
			MACAddress: nodeMAC, DevicePIN: *pin, MeshID: *mesh, Role: "root",
			FirmwareVersion: "sim-1.0.0", LocalIP: "192.168.1.45", WifiRSSI: -60, Timestamp: time.Now().Unix(),
		})
		if err != nil {
			log.Fatalf("handshake: %v", err)
		}
		log.Printf("handshake sent for %s", nodeMAC)
	}
	publish := func(ts time.Time, amps float64) error {
		return send("sentrix/dt/"+*mesh+"/batch", batch{
			MeshID: *mesh, Timestamp: ts.Unix(), RootMAC: nodeMAC,
			Readings: []batchReading{{
				MAC: nodeMAC, NodeType: "root", RMSCurrent: round2(amps),
				CurrentTemperature: 24.5, TargetTemperature: 24.0,
			}},
			OfflineNodes: []string{},
		})
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	now := time.Now().Truncate(time.Minute)

	if *backfill > 0 {
		n := *backfill * 24 * 60
		start := now.Add(-time.Duration(n) * time.Minute)
		log.Printf("backfilling %d readings for %s", n, *mac)
		for i := 0; i < n; i++ {
			select {
			case <-stop:
				return
			default:
			}
			ts := start.Add(time.Duration(i) * time.Minute)
			if err := publish(ts, normalCurrent(ts, rng)); err != nil {
				log.Fatalf("publish: %v", err)
			}
			time.Sleep(*delay)
		}
		log.Println("backfill done")
	}

	if !*live {
		return
	}

	log.Printf("live: one reading every %s (ctrl+c to stop)", *interval)
	tick := time.NewTicker(*interval)
	defer tick.Stop()
	// Live timestamps advance one simulated minute per tick, continuing after the
	// backfill. With a fast -interval they run ahead of wall-clock time; that is fine
	// for testing, and keeps every (device, timestamp) unique.
	ts := now
	if *backfill > 0 {
		ts = now.Add(time.Minute)
	}
	for count := 1; ; count++ {
		select {
		case <-stop:
			return
		case <-tick.C:
		}
		amps := normalCurrent(ts, rng)
		if *spikeEvery > 0 && count%*spikeEvery == 0 {
			amps = *spikeAmps
			log.Printf("spike: %.2fA", amps)
		}
		if err := publish(ts, amps); err != nil {
			log.Printf("publish: %v", err)
		}
		ts = ts.Add(time.Minute)
	}
}

// normalCurrent models a circuit with a daily rhythm: low overnight, a plateau
// during working hours (07:00-18:00 local), a lower weekend load, and a little noise.
func normalCurrent(ts time.Time, rng *rand.Rand) float64 {
	local := ts.Local()
	h := float64(local.Hour()) + float64(local.Minute())/60

	base := 1.0
	if h >= 7 && h < 18 {
		base = 5.0
	} else if h >= 18 && h < 22 {
		base = 3.0
	}
	// Smooth the edges and add a slow daily wave so it isn't a square pulse.
	base += 0.8 * math.Sin(2*math.Pi*(h-6)/24)

	if wd := local.Weekday(); wd == time.Saturday || wd == time.Sunday {
		base *= 0.5
	}
	amps := base + rng.NormFloat64()*0.25
	if amps < 0.05 {
		amps = 0.05
	}
	return amps
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
