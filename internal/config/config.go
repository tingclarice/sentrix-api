package config

import (
	"fmt"
	"os"
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
