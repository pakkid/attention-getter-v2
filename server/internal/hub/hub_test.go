package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"attention-getter/server/internal/push"
	"attention-getter/server/internal/store"
)

type fakeNotifier struct {
	sent chan sent
}

type sent struct {
	emails []string
	msg    push.Message
}

func (f *fakeNotifier) Send(_ context.Context, emails []string, msg push.Message) {
	slices.Sort(emails)
	f.sent <- sent{emails, msg}
}

func setup(t *testing.T) (*Hub, *store.Store, *fakeNotifier, int64, int64) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	for _, u := range []struct{ email, pref string }{
		{"mom@x.com", "mine"}, {"dad@x.com", "mine"}, {"sis@x.com", "all"}, {"bro@x.com", "none"},
	} {
		if err := st.UpsertUser(ctx, u.email, "user"); err != nil {
			t.Fatal(err)
		}
		if err := st.SetNotifyPref(ctx, u.email, u.pref); err != nil {
			t.Fatal(err)
		}
	}
	code, _ := st.CreatePairingCode(ctx, "mom@x.com")
	p, err := st.PairDevice(ctx, code, "desktop", "")
	if err != nil {
		t.Fatal(err)
	}
	n := &fakeNotifier{sent: make(chan sent, 4)}
	return New(st, n), st, n, p.DeviceID, p.InstallID
}

func recv(t *testing.T, c *Conn) ServerMsg {
	t.Helper()
	select {
	case b := <-c.Send:
		var m ServerMsg
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return m
	case <-time.After(time.Second):
		t.Fatal("no message")
	}
	return ServerMsg{}
}

func TestMergeAndNotifyRouting(t *testing.T) {
	h, st, n, dev, inst := setup(t)
	ctx := context.Background()
	conn := h.Attach(ctx, dev, inst)

	keyOwnerKey, _ := st.CreateAPIKey(ctx, "Alexa", "dad@x.com", nil, nil)
	k, err := st.APIKeyBySecret(ctx, keyOwnerKey)
	if err != nil {
		t.Fatal(err)
	}

	a1, err := h.Trigger(ctx, dev, nil, store.Requester{Email: "mom@x.com", Name: "Mom"}, "dinner")
	if err != nil {
		t.Fatal(err)
	}
	if m := recv(t, conn); m.Op != "alert" || m.Alert.ID != a1.ID || m.Alert.Requests[0].Message != "dinner" || len(m.Alert.Presets) != 4 {
		t.Fatalf("unexpected first message: %+v", m)
	}

	a2, err := h.Trigger(ctx, dev, nil, store.Requester{Email: k.OwnerEmail, APIKeyID: k.ID, Name: k.Name}, "")
	if err != nil {
		t.Fatal(err)
	}
	if a2.ID != a1.ID || len(a2.Requests) != 2 {
		t.Fatalf("expected merge into alert %d, got %+v", a1.ID, a2)
	}
	if m := recv(t, conn); m.Op != "alert" || m.Alert.ID != a1.ID || len(m.Alert.Requests) != 2 || m.Alert.Requests[1].Name != "Alexa" {
		t.Fatalf("unexpected merge message: %+v", m)
	}

	if err := h.HandleDeviceMessage(ctx, dev, inst, []byte(`{"op":"ack","alert_id":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := h.HandleDeviceMessage(ctx, dev, inst, []byte(`{"op":"reply","alert_id":1,"text":"5 min"}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case s := <-n.sent:
		// mom (requester, mine) + dad (key owner, mine) + sis (all); bro opted out.
		if !slices.Equal(s.emails, []string{"dad@x.com", "mom@x.com", "sis@x.com"}) {
			t.Fatalf("recipients = %v", s.emails)
		}
		if s.msg.Title != "desktop" || s.msg.Body != "5 min" {
			t.Fatalf("message = %+v", s.msg)
		}
	case <-time.After(time.Second):
		t.Fatal("no notification")
	}

	// A duplicate reply after reconnect is ignored; a new trigger opens a fresh alert.
	if err := h.HandleDeviceMessage(ctx, dev, inst, []byte(`{"op":"reply","alert_id":1,"text":"again"}`)); err != nil {
		t.Fatal(err)
	}
	a3, err := h.Trigger(ctx, dev, nil, store.Requester{Email: "sis@x.com", Name: "Sis"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if a3.ID == a1.ID {
		t.Fatal("expected a new alert after resolution")
	}
}

func TestOfflineDeliveryOnReconnect(t *testing.T) {
	h, st, _, dev, inst := setup(t)
	ctx := context.Background()
	if err := st.SaveSettings(ctx, store.Settings{DeliverOffline: true}); err != nil {
		t.Fatal(err)
	}
	a, err := h.Trigger(ctx, dev, nil, store.Requester{Email: "mom@x.com", Name: "Mom"}, "hi")
	if err != nil {
		t.Fatal(err)
	}
	if h.Online(dev) {
		t.Fatal("device should be offline")
	}
	conn := h.Attach(ctx, dev, inst)
	if m := recv(t, conn); m.Op != "alert" || m.Alert.ID != a.ID {
		t.Fatalf("expected queued alert on connect, got %+v", m)
	}
}

func TestCancel(t *testing.T) {
	h, st, _, dev, inst := setup(t)
	ctx := context.Background()
	conn := h.Attach(ctx, dev, inst)
	a, _ := h.Trigger(ctx, dev, nil, store.Requester{Email: "mom@x.com", Name: "Mom"}, "")
	recv(t, conn)
	if err := h.Cancel(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if m := recv(t, conn); m.Op != "cancel" || m.AlertID != a.ID {
		t.Fatalf("expected cancel, got %+v", m)
	}
	got, _ := st.GetAlert(ctx, a.ID)
	if got.Status != store.StatusCancelled {
		t.Fatalf("status = %s", got.Status)
	}
}

func TestOfflineMissedByDefault(t *testing.T) {
	h, st, _, dev, inst := setup(t)
	ctx := context.Background()
	a, err := h.Trigger(ctx, dev, nil, store.Requester{Email: "mom@x.com", Name: "Mom"}, "hi")
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != store.StatusMissed || a.Resolved == 0 || len(a.Requests) != 1 {
		t.Fatalf("expected a resolved missed alert, got %+v", a)
	}
	// A second trigger while offline is its own missed alert, not merged.
	b, _ := h.Trigger(ctx, dev, nil, store.Requester{Email: "dad@x.com", Name: "Dad"}, "")
	if b.ID == a.ID || b.Status != store.StatusMissed {
		t.Fatalf("expected a separate missed alert, got %+v", b)
	}
	conn := h.Attach(ctx, dev, inst)
	select {
	case m := <-conn.Send:
		t.Fatalf("missed alerts must not be delivered on reconnect, got %s", m)
	case <-time.After(50 * time.Millisecond):
	}
	if _, err := st.OpenAlert(ctx, dev); err != store.ErrNotFound {
		t.Fatalf("expected no open alert, got %v", err)
	}
}

func TestExpireOfflineWhenSettingTurnedOff(t *testing.T) {
	h, st, _, dev, inst := setup(t)
	ctx := context.Background()
	_ = st.SaveSettings(ctx, store.Settings{DeliverOffline: true})
	queued, _ := h.Trigger(ctx, dev, nil, store.Requester{Email: "mom@x.com", Name: "Mom"}, "")
	if queued.Status != store.StatusPending {
		t.Fatalf("expected pending, got %s", queued.Status)
	}
	_ = st.SaveSettings(ctx, store.Settings{DeliverOffline: false})
	if err := h.ExpireOffline(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetAlert(ctx, queued.ID)
	if got.Status != store.StatusMissed {
		t.Fatalf("status = %s, want missed", got.Status)
	}
	// An online PC's alerts are delivered normally with the setting off.
	conn := h.Attach(ctx, dev, inst)
	a, _ := h.Trigger(ctx, dev, nil, store.Requester{Email: "mom@x.com", Name: "Mom"}, "")
	if m := recv(t, conn); m.Op != "alert" || m.Alert.ID != a.ID {
		t.Fatalf("expected live delivery, got %+v", m)
	}
}

func TestDualBootInstalls(t *testing.T) {
	h, st, _, dev, linux := setup(t)
	ctx := context.Background()
	// The Windows side pairs as its own PC first, then gets merged into desktop.
	code, _ := st.CreatePairingCode(ctx, "mom@x.com")
	win, err := st.PairDevice(ctx, code, "desktop-win", "")
	if err != nil {
		t.Fatal(err)
	}
	wconn := h.Attach(ctx, win.DeviceID, win.InstallID)
	_ = st.SaveSettings(ctx, store.Settings{DeliverOffline: true})
	stale, _ := h.Trigger(ctx, win.DeviceID, nil, store.Requester{Email: "mom@x.com", Name: "Mom"}, "")
	recv(t, wconn)
	_ = st.SaveSettings(ctx, store.Settings{DeliverOffline: false})

	if err := h.Merge(ctx, win.DeviceID, dev); err != nil {
		t.Fatal(err)
	}
	if m := recv(t, wconn); m.Op != "cancel" || m.AlertID != stale.ID {
		t.Fatalf("merged-away PC's open alert should be cancelled, got %+v", m)
	}
	select {
	case <-wconn.Done:
	case <-time.After(time.Second):
		t.Fatal("merged-away connection should be closed")
	}
	if h.Online(win.DeviceID) || h.Online(dev) {
		t.Fatal("nothing should be online right after the merge")
	}
	if d, err := st.DeviceByToken(ctx, win.Token); err != nil || d.ID != dev || d.InstallID != win.InstallID {
		t.Fatalf("windows token should now open desktop: %+v, %v", d, err)
	}

	// Windows boots: the PC is online and gets alerts, the Linux install is not connected.
	wconn = h.Attach(ctx, dev, win.InstallID)
	if !h.Online(dev) || h.InstallOnline(dev, linux) || !h.InstallOnline(dev, win.InstallID) {
		t.Fatal("expected only the windows install online")
	}
	a, _ := h.Trigger(ctx, dev, nil, store.Requester{Email: "mom@x.com", Name: "Mom"}, "")
	if m := recv(t, wconn); m.Op != "alert" || m.Alert.ID != a.ID {
		t.Fatalf("expected alert on windows, got %+v", m)
	}

	// Both connected at once (e.g. a stale socket): a reply on one closes the popup on the other.
	lconn := h.Attach(ctx, dev, linux)
	if m := recv(t, lconn); m.Op != "alert" || m.Alert.ID != a.ID {
		t.Fatalf("expected the open alert on linux too, got %+v", m)
	}
	if err := h.HandleDeviceMessage(ctx, dev, win.InstallID, []byte(fmt.Sprintf(`{"op":"reply","alert_id":%d,"text":"ok"}`, a.ID))); err != nil {
		t.Fatal(err)
	}
	if m := recv(t, lconn); m.Op != "cancel" || m.AlertID != a.ID {
		t.Fatalf("expected cancel on linux, got %+v", m)
	}
	select {
	case m := <-wconn.Send:
		t.Fatalf("the install that replied needs no cancel, got %s", m)
	default:
	}
	// A reconnect of one install doesn't drop the other.
	lconn2 := h.Attach(ctx, dev, linux)
	if !h.InstallOnline(dev, win.InstallID) {
		t.Fatal("windows should still be connected")
	}
	h.Detach(dev, linux, lconn) // the replaced socket closing late changes nothing
	if !h.InstallOnline(dev, linux) {
		t.Fatal("linux reconnection should survive the old socket's detach")
	}
	h.Detach(dev, linux, lconn2)
	h.Detach(dev, win.InstallID, wconn)
	if h.Online(dev) {
		t.Fatal("expected offline")
	}
}
