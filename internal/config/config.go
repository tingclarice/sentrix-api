package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	DatabaseURL      string
	MQTTBrokerURL    string
	MQTTUsername     string
	MQTTPassword     string
	PythonServiceURL string
	InternalAPIKey   string

	// HTTP API.
	HTTPPort       string
	AllowedOrigins []string

	// CalibrationMinutes is how long a newly claimed device calibrates (7 days = 10080).
	CalibrationMinutes int

	// Web Push. Leave the keys empty to disable sending (subscriptions are still stored).
	// VAPIDSubject identifies the sender to push services: a mailto: or https: URL.
	VAPIDPublicKey  string
	VAPIDPrivateKey string
	VAPIDSubject    string

	// Supabase JWT verification. Set SupabaseJWTSecret for projects that sign with
	// the legacy shared secret (HS256), and SupabaseURL for projects that sign with
	// an asymmetric key (ES256/RS256, verified against the project's JWKS).
	SupabaseURL       string
	SupabaseJWTSecret string
}

func Load() (Config, error) {
	c := Config{
		DatabaseURL:      os.Getenv("DATABASE_URL"),
		MQTTBrokerURL:    os.Getenv("MQTT_BROKER_URL"),
		MQTTUsername:     os.Getenv("MQTT_USERNAME"),
		MQTTPassword:     os.Getenv("MQTT_PASSWORD"),
		PythonServiceURL: os.Getenv("PYTHON_SERVICE_URL"),
		InternalAPIKey:   os.Getenv("INTERNAL_API_KEY"),

		HTTPPort:          envOr("PORT", "8080"),
		AllowedOrigins:    splitList(envOr("ALLOWED_ORIGINS", "http://localhost:3000")),
		SupabaseURL:       strings.TrimRight(os.Getenv("SUPABASE_URL"), "/"),
		SupabaseJWTSecret: os.Getenv("SUPABASE_JWT_SECRET"),
		VAPIDPublicKey:    os.Getenv("VAPID_PUBLIC_KEY"),
		VAPIDPrivateKey:   os.Getenv("VAPID_PRIVATE_KEY"),
		VAPIDSubject:      os.Getenv("VAPID_SUBJECT"),
	}
	minutes, err := strconv.Atoi(envOr("CALIBRATION_MINUTES", "10080"))
	if err != nil || minutes <= 0 {
		return c, fmt.Errorf("CALIBRATION_MINUTES must be a positive integer")
	}
	c.CalibrationMinutes = minutes
	if err := c.validatePush(); err != nil {
		return c, err
	}
	if c.DatabaseURL == "" || c.MQTTBrokerURL == "" {
		return c, fmt.Errorf("DATABASE_URL and MQTT_BROKER_URL are required")
	}
	return c, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// splitList splits a comma-separated list, trimming blanks.
func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// PushEnabled reports whether Web Push sending is configured.
func (c Config) PushEnabled() bool { return c.VAPIDPublicKey != "" && c.VAPIDPrivateKey != "" }

func (c Config) validatePush() error {
	if (c.VAPIDPublicKey == "") != (c.VAPIDPrivateKey == "") {
		return fmt.Errorf("VAPID_PUBLIC_KEY and VAPID_PRIVATE_KEY must be set together")
	}
	if c.PushEnabled() && !strings.HasPrefix(c.VAPIDSubject, "mailto:") && !strings.HasPrefix(c.VAPIDSubject, "https://") {
		return fmt.Errorf("VAPID_SUBJECT must be a mailto: or https:// URL when Web Push is enabled")
	}
	return nil
}
