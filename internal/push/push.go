// Package push sends Web Push notifications for new alerts.
package push

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/SherClockHolmes/webpush-go"

	"github.com/tingclarice/sentrix-api/internal/store"
)

// Alert is what a notification needs to say. Ingest builds one when it inserts an alert.
type Alert struct {
	ID         string
	DeviceID   string
	DeviceName string // may be empty
	DeviceMAC  string
	Severity   string
	Value      float64
	Min, Max   float64
}

// Subscriptions is the part of the store the sender needs.
type Subscriptions interface {
	PushSubscriptionsForDevice(ctx context.Context, deviceID string) ([]store.PushSubscription, error)
	DeletePushSubscriptionByEndpoint(ctx context.Context, endpoint string) error
}

type Sender struct {
	subs        Subscriptions
	public      string
	private     string
	subject     string
	client      webpush.HTTPClient
	sendTimeout time.Duration
}

func New(subs Subscriptions, publicKey, privateKey, subject string) *Sender {
	return &Sender{
		subs: subs, public: publicKey, private: privateKey, subject: subject,
		client:      &http.Client{Timeout: 10 * time.Second},
		sendTimeout: 30 * time.Second,
	}
}

// payload is the JSON the service worker receives in its "push" event.
type payload struct {
	Title    string `json:"title"`
	Body     string `json:"body"`
	AlertID  string `json:"alert_id"`
	DeviceID string `json:"device_id"`
	Severity string `json:"severity"`
	URL      string `json:"url"`
}

func buildPayload(a Alert) ([]byte, error) {
	name := a.DeviceName
	if name == "" {
		name = a.DeviceMAC
	}
	return json.Marshal(payload{
		Title:    fmt.Sprintf("%s alert: %s", a.Severity, name),
		Body:     fmt.Sprintf("%.2fA, expected %.2f-%.2fA", a.Value, a.Min, a.Max),
		AlertID:  a.ID,
		DeviceID: a.DeviceID,
		Severity: a.Severity,
		URL:      "/alerts",
	})
}

// Notify sends the alert to every subscription of the device's owner. It returns at
// once and does the work in the background, so a slow push service never delays ingest.
func (s *Sender) Notify(a Alert) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), s.sendTimeout)
		defer cancel()
		if err := s.Send(ctx, a); err != nil {
			log.Printf("push: alert %s: %v", a.ID, err)
		}
	}()
}

// Send delivers the alert synchronously. A subscription the push service reports as
// gone (404 or 410) is deleted; other failures are logged and do not stop the rest.
func (s *Sender) Send(ctx context.Context, a Alert) error {
	subs, err := s.subs.PushSubscriptionsForDevice(ctx, a.DeviceID)
	if err != nil {
		return fmt.Errorf("load subscriptions: %w", err)
	}
	if len(subs) == 0 {
		return nil
	}
	msg, err := buildPayload(a)
	if err != nil {
		return err
	}
	urgency := webpush.UrgencyNormal
	if a.Severity == "CRITICAL" {
		urgency = webpush.UrgencyHigh
	}

	for _, sub := range subs {
		resp, err := webpush.SendNotificationWithContext(ctx, msg, &webpush.Subscription{
			Endpoint: sub.Endpoint,
			Keys:     webpush.Keys{P256dh: sub.P256dh, Auth: sub.Auth},
		}, &webpush.Options{
			HTTPClient:      s.client,
			Subscriber:      s.subject,
			VAPIDPublicKey:  s.public,
			VAPIDPrivateKey: s.private,
			TTL:             3600, // an alert older than an hour is not worth delivering
			Urgency:         urgency,
		})
		if err != nil {
			log.Printf("push: send to %.40s...: %v", sub.Endpoint, err)
			continue
		}
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
			if err := s.subs.DeletePushSubscriptionByEndpoint(ctx, sub.Endpoint); err != nil {
				log.Printf("push: delete expired subscription: %v", err)
			}
		case resp.StatusCode >= 300:
			log.Printf("push: %.40s... answered %d", sub.Endpoint, resp.StatusCode)
		}
	}
	return nil
}
