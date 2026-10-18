package config

import (
	"fmt"
	"os"
)

type Config struct {
	DatabaseURL      string
	MQTTBrokerURL    string
	MQTTUsername     string
	MQTTPassword     string
	PythonServiceURL string
	InternalAPIKey   string
}

func Load() (Config, error) {
	c := Config{
		DatabaseURL:      os.Getenv("DATABASE_URL"),
		MQTTBrokerURL:    os.Getenv("MQTT_BROKER_URL"),
		MQTTUsername:     os.Getenv("MQTT_USERNAME"),
		MQTTPassword:     os.Getenv("MQTT_PASSWORD"),
		PythonServiceURL: os.Getenv("PYTHON_SERVICE_URL"),
		InternalAPIKey:   os.Getenv("INTERNAL_API_KEY"),
	}
	if c.DatabaseURL == "" || c.MQTTBrokerURL == "" {
		return c, fmt.Errorf("DATABASE_URL and MQTT_BROKER_URL are required")
	}
	return c, nil
}
