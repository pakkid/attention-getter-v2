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

func (f *fakeNotifier) Send(_ context.Context, emails []string, msg push.Message) int {
	slices.Sort(emails)
	f.sent <- sent{emails, msg}
	return len(emails)
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

func TestNotifyAutomationOptIn(t *testing.T) {
	h, st, n, dev, inst := setup(t)
	ctx := context.Background()
	h.Attach(ctx, dev, inst)
	// mom opts in; bro opts in too but his pref is 'none', so it has no effect.
	for _, e := range []string{"mom@x.com", "bro@x.com"} {
		if err := st.SetNotifyAutomation(ctx, e, true); err != nil {
			t.Fatal(err)
		}
	}
	secret, _ := st.CreateAPIKey(ctx, "Alexa", "dad@x.com", nil, nil)
	k, _ := st.APIKeyBySecret(ctx, secret)

	reply := func(req store.Requester) []string {
		t.Helper()
		a, err := h.Trigger(ctx, dev, nil, req, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := h.HandleDeviceMessage(ctx, dev, inst, []byte(fmt.Sprintf(`{"op":"reply","alert_id":%d,"text":"ok"}`, a.ID))); err != nil {
			t.Fatal(err)
		}
		select {
		case s := <-n.sent:
			return s.emails
		case <-time.After(time.Second):
			t.Fatal("no notification")
		}
		return nil
	}

	if got := reply(store.Requester{Email: k.OwnerEmail, APIKeyID: k.ID, Name: k.Name}); !slices.Equal(got, []string{"dad@x.com", "mom@x.com", "sis@x.com"}) {
		t.Fatalf("api-key trigger recipients = %v", got)
	}
	// A trigger from a person doesn't reach mom: she only opted into automation replies.
	if got := reply(store.Requester{Email: "dad@x.com", Name: "Dad"}); !slices.Equal(got, []string{"dad@x.com", "sis@x.com"}) {
		t.Fatalf("user trigger recipients = %v", got)
	}
}

func nextPush(t *testing.T, n *fakeNotifier) sent {
	t.Helper()
	select {
	case s := <-n.sent:
		return s
	case <-time.After(time.Second):
		t.Fatal("no notification")
	}
	return sent{}
}

func waitStatus(t *testing.T, st *store.Store, id int64, want string) {
	t.Helper()
	for range 100 {
		if a, _ := st.GetAlert(context.Background(), id); a != nil && a.Status == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("alert %d never became %s", id, want)
}

func TestPhone(t *testing.T) {
	h, st, n, _, _ := setup(t)
	ctx := context.Background()
	if err := st.UpsertUser(ctx, "kid@x.com", "user"); err != nil {
		t.Fatal(err)
	}
	_ = st.SetNotifyPref(ctx, "kid@x.com", "all")
	phone, err := st.CreatePhone(ctx, "Kid's phone", "kid@x.com")
	if err != nil {
		t.Fatal(err)
	}

	// No notifications turned on: not sent, whatever the offline setting says.
	_ = st.SaveSettings(ctx, store.Settings{DeliverOffline: true})
	a, err := h.Trigger(ctx, phone, nil, store.Requester{Email: "mom@x.com", Name: "Mom"}, "dinner")
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != store.StatusMissed || a.Phone != "kid@x.com" {
		t.Fatalf("expected a missed phone alert, got %+v", a)
	}

	if err := st.AddPushSub(ctx, "kid@x.com", store.PushSub{Endpoint: "https://push.example/1", P256dh: "k", Auth: "a"}); err != nil {
		t.Fatal(err)
	}
	a, err = h.Trigger(ctx, phone, nil, store.Requester{Email: "mom@x.com", Name: "Mom"}, "dinner")
	if err != nil {
		t.Fatal(err)
	}
	s := nextPush(t, n)
	if !slices.Equal(s.emails, []string{"kid@x.com"}) || s.msg.Title != "ATTENTION!" || s.msg.Body != "Mom: “dinner”" ||
		s.msg.URL != fmt.Sprintf("/#reply/%d", a.ID) {
		t.Fatalf("ring = %+v", s)
	}
	waitStatus(t, st, a.ID, store.StatusDelivered)

	b, _ := h.Trigger(ctx, phone, nil, store.Requester{Email: "dad@x.com", Name: "Dad"}, "")
	if b.ID != a.ID {
		t.Fatalf("expected merge into %d, got %d", a.ID, b.ID)
	}
	if s := nextPush(t, n); s.msg.Body != "Mom: “dinner”\nDad" {
		t.Fatalf("re-ring = %+v", s.msg)
	}

	if err := h.PhoneReply(ctx, a.ID, "mom@x.com", store.StatusReplied, "hi"); err != ErrNotYours {
		t.Fatalf("someone else's reply: err = %v", err)
	}
	if err := h.PhoneReply(ctx, a.ID, "kid@x.com", store.StatusReplied, "coming"); err != nil {
		t.Fatal(err)
	}
	// The requesters and sis ('all') hear about it; kid is 'all' too but answered it.
	if s := nextPush(t, n); !slices.Equal(s.emails, []string{"dad@x.com", "mom@x.com", "sis@x.com"}) || s.msg.Body != "coming" {
		t.Fatalf("outcome = %+v", s)
	}
	if err := h.PhoneReply(ctx, a.ID, "kid@x.com", store.StatusReplied, "again"); err != store.ErrNotOpen {
		t.Fatalf("second reply: err = %v", err)
	}

	// Cancelling quietly replaces the notification.
	c, _ := h.Trigger(ctx, phone, nil, store.Requester{Email: "mom@x.com", Name: "Mom"}, "")
	nextPush(t, n)
	if err := h.Cancel(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if s := nextPush(t, n); !s.msg.Silent || s.msg.Tag != fmt.Sprintf("alert-%d", c.ID) {
		t.Fatalf("cancel = %+v", s.msg)
	}

	// A PC can't be paired under the phone's name.
	code, _ := st.CreatePairingCode(ctx, "mom@x.com")
	if _, err := st.PairDevice(ctx, code, "Kid's phone", ""); err != store.ErrConflict {
		t.Fatalf("pairing as the phone: err = %v", err)
	}
}
