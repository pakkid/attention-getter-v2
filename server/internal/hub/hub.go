// Package hub routes alerts between triggers, connected PCs and web clients.
package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"attention-getter/server/internal/push"
	"attention-getter/server/internal/store"
)

type Notifier interface {
	Send(ctx context.Context, emails []string, msg push.Message)
}

// Conn is one connected PC. The WebSocket writer drains Send until Done closes.
type Conn struct {
	Send chan []byte
	Done chan struct{}
	once sync.Once
}

func (c *Conn) close() { c.once.Do(func() { close(c.Done) }) }

type Hub struct {
	st     *store.Store
	notify Notifier

	mu      sync.Mutex
	devices map[int64]*Conn
	subs    map[chan []byte]struct{}
}

func New(st *store.Store, n Notifier) *Hub {
	return &Hub{st: st, notify: n, devices: map[int64]*Conn{}, subs: map[chan []byte]struct{}{}}
}

// ---- wire format (server <-> PC) ----

type TypeRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Hash string `json:"hash"`
}

type DeviceRequest struct {
	Name    string `json:"name"`
	Message string `json:"message"`
}

type DeviceAlert struct {
	ID       int64           `json:"id"`
	Type     *TypeRef        `json:"type"`
	Requests []DeviceRequest `json:"requests"`
	Presets  []string        `json:"presets"`
}

type ServerMsg struct {
	Op      string       `json:"op"` // alert | cancel | manifest_changed
	Alert   *DeviceAlert `json:"alert,omitempty"`
	AlertID int64        `json:"alert_id,omitempty"`
}

type DeviceMsg struct {
	Op      string `json:"op"` // ack | reply | dismiss
	AlertID int64  `json:"alert_id"`
	Text    string `json:"text"`
}

// ---- device connections ----

// Attach registers a PC connection, replacing any previous one, and queues its open alert.
func (h *Hub) Attach(ctx context.Context, deviceID int64) *Conn {
	c := &Conn{Send: make(chan []byte, 16), Done: make(chan struct{})}
	h.mu.Lock()
	if old := h.devices[deviceID]; old != nil {
		old.close()
	}
	h.devices[deviceID] = c
	if a, err := h.st.OpenAlert(ctx, deviceID); err == nil {
		if msg, err := h.alertMsg(ctx, a); err == nil {
			h.sendLocked(deviceID, msg)
		}
	}
	h.mu.Unlock()
	h.broadcast(map[string]any{"type": "devices"})
	return c
}

func (h *Hub) Detach(deviceID int64, c *Conn) {
	h.mu.Lock()
	if h.devices[deviceID] == c {
		delete(h.devices, deviceID)
	}
	h.mu.Unlock()
	c.close()
	h.broadcast(map[string]any{"type": "devices"})
}

// Kick disconnects a PC (deleted or re-paired).
func (h *Hub) Kick(deviceID int64) {
	h.mu.Lock()
	c := h.devices[deviceID]
	delete(h.devices, deviceID)
	h.mu.Unlock()
	if c != nil {
		c.close()
		h.broadcast(map[string]any{"type": "devices"})
	}
}

func (h *Hub) Online(deviceID int64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.devices[deviceID] != nil
}

// sendLocked queues msg for a device; a PC too slow to drain its queue is disconnected.
func (h *Hub) sendLocked(deviceID int64, msg ServerMsg) {
	c := h.devices[deviceID]
	if c == nil {
		return
	}
	b, _ := json.Marshal(msg)
	select {
	case c.Send <- b:
	default:
		c.close()
		delete(h.devices, deviceID)
	}
}

// ManifestChanged tells every PC to resync media now instead of at the next poll.
func (h *Hub) ManifestChanged() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id := range h.devices {
		h.sendLocked(id, ServerMsg{Op: "manifest_changed"})
	}
}

func (h *Hub) alertMsg(ctx context.Context, a *store.Alert) (ServerMsg, error) {
	presets, err := h.st.Presets(ctx)
	if err != nil {
		return ServerMsg{}, err
	}
	da := &DeviceAlert{ID: a.ID, Presets: presets, Requests: []DeviceRequest{}}
	if a.TypeID != nil {
		if t, err := h.st.GetType(ctx, *a.TypeID); err == nil {
			da.Type = &TypeRef{ID: t.ID, Name: t.Name, Hash: t.Hash}
		}
	}
	for _, r := range a.Requests {
		da.Requests = append(da.Requests, DeviceRequest{Name: r.Name, Message: r.Message})
	}
	return ServerMsg{Op: "alert", Alert: da}, nil
}

// ---- alert lifecycle ----

// Trigger raises an alert on a PC, merging into its open alert if one is showing.
func (h *Hub) Trigger(ctx context.Context, deviceID int64, typeID *int64, r store.Requester, message string) (*store.Alert, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	open, err := h.st.OpenAlert(ctx, deviceID)
	switch {
	case err == nil:
		if err := h.st.AddAlertRequest(ctx, open.ID, r, message); err != nil {
			return nil, err
		}
		// Resend the whole alert; the PC updates its popup in place when the id matches.
		open, err = h.st.GetAlert(ctx, open.ID)
		if err != nil {
			return nil, err
		}
		msg, err := h.alertMsg(ctx, open)
		if err != nil {
			return nil, err
		}
		h.sendLocked(deviceID, msg)
		h.broadcastAlert(open)
		return open, nil
	case errors.Is(err, store.ErrNotFound):
	default:
		return nil, err
	}

	id, err := h.st.CreateAlert(ctx, deviceID, typeID)
	if err != nil {
		return nil, err
	}
	if err := h.st.AddAlertRequest(ctx, id, r, message); err != nil {
		return nil, err
	}
	a, err := h.st.GetAlert(ctx, id)
	if err != nil {
		return nil, err
	}
	msg, err := h.alertMsg(ctx, a)
	if err != nil {
		return nil, err
	}
	h.sendLocked(deviceID, msg)
	h.broadcastAlert(a)
	return a, nil
}

// HandleDeviceMessage processes an ack, reply or dismiss from a PC.
func (h *Hub) HandleDeviceMessage(ctx context.Context, deviceID int64, raw []byte) error {
	var m DeviceMsg
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	switch m.Op {
	case "ack":
		if err := h.st.MarkDelivered(ctx, m.AlertID, deviceID); err != nil {
			return err
		}
		if a, err := h.st.GetAlert(ctx, m.AlertID); err == nil {
			h.broadcastAlert(a)
		}
	case "reply", "dismiss":
		status, text := store.StatusReplied, truncate(m.Text, 500)
		if m.Op == "dismiss" {
			status, text = store.StatusDismissed, ""
		}
		err := h.st.ResolveAlert(ctx, m.AlertID, deviceID, status, text)
		if errors.Is(err, store.ErrNotOpen) {
			return nil // duplicate after reconnect, or cancelled meanwhile
		}
		if err != nil {
			return err
		}
		a, err := h.st.GetAlert(ctx, m.AlertID)
		if err != nil {
			return err
		}
		h.broadcastAlert(a)
		go h.notifyOutcome(a)
	default:
		return fmt.Errorf("unknown op %q", m.Op)
	}
	return nil
}

// Cancel withdraws an open alert from the web app and closes the popup.
func (h *Hub) Cancel(ctx context.Context, alertID int64) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	a, err := h.st.GetAlert(ctx, alertID)
	if err != nil {
		return err
	}
	if err := h.st.ResolveAlert(ctx, alertID, 0, store.StatusCancelled, ""); err != nil {
		return err
	}
	h.sendLocked(a.DeviceID, ServerMsg{Op: "cancel", AlertID: alertID})
	if a, err = h.st.GetAlert(ctx, alertID); err == nil {
		h.broadcastAlert(a)
	}
	return nil
}

func (h *Hub) notifyOutcome(a *store.Alert) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	emails, err := h.st.NotifyRecipients(ctx, a.ID)
	if err != nil {
		slog.Error("notify recipients", "err", err)
		return
	}
	h.notify.Send(ctx, emails, OutcomeMessage(a))
}

// OutcomeMessage is the push notification shown on phones for a resolved alert.
func OutcomeMessage(a *store.Alert) push.Message {
	body := a.Reply
	if a.Status == store.StatusDismissed {
		body = "Dismissed without a reply"
	}
	return push.Message{
		Title: a.DeviceName,
		Body:  body,
		Tag:   fmt.Sprintf("alert-%d", a.ID),
		URL:   "/#history",
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// ---- web (SSE) subscribers ----

func (h *Hub) Subscribe() chan []byte {
	ch := make(chan []byte, 16)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *Hub) Unsubscribe(ch chan []byte) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
}

func (h *Hub) broadcastAlert(a *store.Alert) {
	h.broadcastLocked(map[string]any{"type": "alert", "alert": a})
}

func (h *Hub) broadcast(v any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.broadcastLocked(v)
}

func (h *Hub) broadcastLocked(v any) {
	b, _ := json.Marshal(v)
	for ch := range h.subs {
		select {
		case ch <- b:
		default: // slow browser; it refetches on reconnect
		}
	}
}
