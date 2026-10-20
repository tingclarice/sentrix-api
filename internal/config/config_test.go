package config

import "testing"

func TestPushConfig(t *testing.T) {
	tests := []struct {
		name    string
		pub     string
		priv    string
		subject string
		wantErr bool
		enabled bool
	}{
		{"disabled", "", "", "", false, false},
		{"fully configured", "pub", "priv", "mailto:ops@example.com", false, true},
		{"https subject", "pub", "priv", "https://example.com", false, true},
		{"only the public key", "pub", "", "mailto:a@b.c", true, false},
		{"only the private key", "", "priv", "mailto:a@b.c", true, false},
		{"missing subject", "pub", "priv", "", true, false},
		{"subject is not a URL", "pub", "priv", "ops@example.com", true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://x")
			t.Setenv("MQTT_BROKER_URL", "tcp://x")
			t.Setenv("VAPID_PUBLIC_KEY", tt.pub)
			t.Setenv("VAPID_PRIVATE_KEY", tt.priv)
			t.Setenv("VAPID_SUBJECT", tt.subject)
			c, err := Load()
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && c.PushEnabled() != tt.enabled {
				t.Errorf("PushEnabled = %v, want %v", c.PushEnabled(), tt.enabled)
			}
		})
	}
}

func TestCalibrationMinutes(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("MQTT_BROKER_URL", "tcp://x")

	t.Setenv("CALIBRATION_MINUTES", "")
	if c, err := Load(); err != nil || c.CalibrationMinutes != 10080 {
		t.Errorf("default = %d, %v, want 10080", c.CalibrationMinutes, err)
	}
	t.Setenv("CALIBRATION_MINUTES", "5")
	if c, err := Load(); err != nil || c.CalibrationMinutes != 5 {
		t.Errorf("5 = %d, %v", c.CalibrationMinutes, err)
	}
	for _, bad := range []string{"0", "-3", "abc"} {
		t.Setenv("CALIBRATION_MINUTES", bad)
		if _, err := Load(); err == nil {
			t.Errorf("CALIBRATION_MINUTES=%q accepted, want an error", bad)
		}
	}
}
