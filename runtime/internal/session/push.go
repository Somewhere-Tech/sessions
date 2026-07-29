package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	webpush "github.com/SherClockHolmes/webpush-go"
)

const vapidSubject = "mailto:sessions@localhost"

type VAPIDKeys struct {
	PublicKey  string `json:"publicKey"`
	PrivateKey string `json:"privateKey"`
}

type PushSubscription struct {
	Endpoint       string          `json:"endpoint"`
	ExpirationTime json.RawMessage `json:"expirationTime,omitempty"`
	Keys           struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

type PushPayload struct {
	Title string         `json:"title"`
	Body  string         `json:"body,omitempty"`
	Data  map[string]any `json:"data,omitempty"`
}

type PushService struct {
	root              string
	vapidPath         string
	subscriptionsPath string
	mu                sync.Mutex
	httpClient        webpush.HTTPClient
}

func NewPushService(stateRoot string) *PushService {
	return &PushService{
		root:              stateRoot,
		vapidPath:         filepath.Join(stateRoot, "vapid.json"),
		subscriptionsPath: filepath.Join(stateRoot, "push-subscriptions.json"),
	}
}

func (p *PushService) SetHTTPClient(client webpush.HTTPClient) { p.httpClient = client }

func (p *PushService) HasSubscriptions() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.loadSubscriptionsLocked()) > 0
}

func (p *PushService) VAPIDPublicKey() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	keys, err := p.vapidKeysLocked()
	return keys.PublicKey, err
}

func (p *PushService) AddSubscription(value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return errors.New("invalid push subscription")
	}
	var subscription PushSubscription
	if json.Unmarshal(raw, &subscription) != nil || !validSubscription(subscription) {
		return errors.New("invalid push subscription")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	subscriptions := p.loadSubscriptionsLocked()
	filtered := subscriptions[:0]
	for _, existing := range subscriptions {
		if existing.Endpoint != subscription.Endpoint {
			filtered = append(filtered, existing)
		}
	}
	return p.saveSubscriptionsLocked(append(filtered, subscription))
}

func (p *PushService) RemoveSubscription(endpoint string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	subscriptions := p.loadSubscriptionsLocked()
	filtered := subscriptions[:0]
	for _, subscription := range subscriptions {
		if subscription.Endpoint != endpoint {
			filtered = append(filtered, subscription)
		}
	}
	return p.saveSubscriptionsLocked(filtered)
}

func (p *PushService) Send(ctx context.Context, payload PushPayload) {
	p.mu.Lock()
	subscriptions := p.loadSubscriptionsLocked()
	if len(subscriptions) == 0 {
		p.mu.Unlock()
		return
	}
	keys, err := p.vapidKeysLocked()
	p.mu.Unlock()
	if err != nil {
		return
	}
	message, err := json.Marshal(payload)
	if err != nil {
		return
	}
	for _, subscription := range subscriptions {
		response, sendErr := webpush.SendNotificationWithContext(ctx, message, &webpush.Subscription{
			Endpoint: subscription.Endpoint,
			Keys:     webpush.Keys{P256dh: subscription.Keys.P256dh, Auth: subscription.Keys.Auth},
		}, &webpush.Options{
			HTTPClient: p.httpClient, Subscriber: vapidSubject,
			VAPIDPublicKey: keys.PublicKey, VAPIDPrivateKey: keys.PrivateKey,
			TTL: 60 * 60, Urgency: webpush.UrgencyNormal,
		})
		status := 0
		if response != nil {
			status = response.StatusCode
			_ = response.Body.Close()
		}
		if (sendErr != nil && (status == http.StatusNotFound || status == http.StatusGone)) ||
			status == http.StatusNotFound || status == http.StatusGone {
			_ = p.RemoveSubscription(subscription.Endpoint)
		}
	}
}

func (p *PushService) vapidKeysLocked() (VAPIDKeys, error) {
	if encoded, err := os.ReadFile(p.vapidPath); err == nil {
		var keys VAPIDKeys
		if json.Unmarshal(encoded, &keys) == nil && keys.PublicKey != "" && keys.PrivateKey != "" {
			return keys, nil
		}
	}
	privateKey, publicKey, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		return VAPIDKeys{}, fmt.Errorf("generate VAPID keys: %w", err)
	}
	keys := VAPIDKeys{PublicKey: publicKey, PrivateKey: privateKey}
	if err := p.writeJSONLocked(p.vapidPath, keys); err != nil {
		return VAPIDKeys{}, err
	}
	return keys, nil
}

func (p *PushService) loadSubscriptionsLocked() []PushSubscription {
	encoded, err := os.ReadFile(p.subscriptionsPath)
	if err != nil {
		return []PushSubscription{}
	}
	var items []json.RawMessage
	if json.Unmarshal(encoded, &items) != nil {
		return []PushSubscription{}
	}
	subscriptions := make([]PushSubscription, 0, len(items))
	for _, item := range items {
		var subscription PushSubscription
		if json.Unmarshal(item, &subscription) == nil && validSubscription(subscription) {
			subscriptions = append(subscriptions, subscription)
		}
	}
	return subscriptions
}

func validSubscription(subscription PushSubscription) bool {
	if subscription.Endpoint == "" || subscription.Keys.P256dh == "" || subscription.Keys.Auth == "" {
		return false
	}
	if len(subscription.ExpirationTime) == 0 || string(subscription.ExpirationTime) == "null" {
		return true
	}
	var number json.Number
	return json.Unmarshal(subscription.ExpirationTime, &number) == nil
}

func (p *PushService) saveSubscriptionsLocked(subscriptions []PushSubscription) error {
	return p.writeJSONLocked(p.subscriptionsPath, subscriptions)
}

func (p *PushService) writeJSONLocked(path string, value any) error {
	if err := os.MkdirAll(p.root, 0o700); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}
