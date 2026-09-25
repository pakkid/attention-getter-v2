// Package push sends Web Push notifications (VAPID) to the PWA.
package push

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"

	"attention-getter/server/internal/store"
)

type Keys struct {
	Public  string `json:"public"`
	Private string `json:"private"`
}

// LoadOrCreateKeys reads VAPID keys from path, generating and saving them on first run.
func LoadOrCreateKeys(path string) (Keys, error) {
	var k Keys
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &k); err != nil {
			return k, err
		}
		return k, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return k, err
	}
	priv, pub, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		return k, err
	}
	k = Keys{Public: pub, Private: priv}
	b, _ := json.Marshal(k)
	return k, os.WriteFile(path, b, 0o600)
}

type Message struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Tag   string `json:"tag,omitempty"`
	URL   string `json:"url,omitempty"`
}

type Service struct {
	keys    Keys
	subject string
	store   *store.Store
	client  *http.Client
}

// New creates a push service. subject is the VAPID contact (https URL or mailto:).
func New(keys Keys, subject string, st *store.Store) *Service {
	return &Service{keys: keys, subject: subject, store: st, client: &http.Client{Timeout: 15 * time.Second}}
}

func (s *Service) PublicKey() string { return s.keys.Public }

// Send delivers msg to every subscription of the given users, pruning expired subscriptions.
func (s *Service) Send(ctx context.Context, emails []string, msg Message) {
	subs, err := s.store.PushSubsFor(ctx, emails)
	if err != nil {
		slog.Error("push: load subscriptions", "err", err)
		return
	}
	payload, _ := json.Marshal(msg)
	for _, sub := range subs {
		resp, err := webpush.SendNotificationWithContext(ctx, payload, &webpush.Subscription{
			Endpoint: sub.Endpoint,
			Keys:     webpush.Keys{P256dh: sub.P256dh, Auth: sub.Auth},
		}, &webpush.Options{
			HTTPClient:      s.client,
			Subscriber:      s.subject,
			VAPIDPublicKey:  s.keys.Public,
			VAPIDPrivateKey: s.keys.Private,
			TTL:             3600,
			Urgency:         webpush.UrgencyHigh,
		})
		if err != nil {
			slog.Warn("push: send", "err", err)
			continue
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
			_ = s.store.DeletePushSub(ctx, sub.Endpoint)
		case resp.StatusCode >= 400:
			slog.Warn("push: rejected", "status", resp.StatusCode)
		}
	}
}
