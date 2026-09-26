package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"attention-getter/server/internal/store"
)

func (s *Server) config(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"google_client_id": s.Google.ClientID,
		"vapid_public_key": s.Push.PublicKey(),
		"dev_login":        s.DevLogin,
	})
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, email string) {
	tok, err := s.St.CreateSession(r.Context(), email, sessionTTL)
	if err != nil {
		internalError(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: tok, Path: "/", MaxAge: int(sessionTTL.Seconds()),
		HttpOnly: true, Secure: s.SecureCookies, SameSite: http.SameSiteLaxMode,
	})
	u, err := s.St.GetUser(r.Context(), email)
	if err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) loginGoogle(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Credential string `json:"credential"`
	}
	if err := readJSON(r, &body); err != nil || body.Credential == "" {
		httpError(w, http.StatusBadRequest, "missing credential")
		return
	}
	id, err := s.Google.Verify(r.Context(), body.Credential)
	if err != nil {
		slog.Warn("google login rejected", "err", err)
		httpError(w, http.StatusUnauthorized, "invalid Google sign-in")
		return
	}
	if _, err := s.St.GetUser(r.Context(), id.Email); err != nil {
		httpError(w, http.StatusForbidden, id.Email+" is not on the allowlist; ask an admin to add you")
		return
	}
	if err := s.St.UpdateProfile(r.Context(), id.Email, id.Name, id.Picture); err != nil {
		internalError(w, err)
		return
	}
	s.startSession(w, r, id.Email)
}

func (s *Server) loginDev(w http.ResponseWriter, r *http.Request) {
	if !s.DevLogin {
		http.NotFound(w, r)
		return
	}
	var body struct {
		Email string `json:"email"`
	}
	if err := readJSON(r, &body); err != nil {
		httpError(w, http.StatusBadRequest, "bad body")
		return
	}
	email := strings.ToLower(strings.TrimSpace(body.Email))
	if _, err := s.St.GetUser(r.Context(), email); err != nil {
		httpError(w, http.StatusForbidden, "not on the allowlist")
		return
	}
	s.startSession(w, r, email)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = s.St.DeleteSession(r.Context(), c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.SecureCookies})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) getMe(w http.ResponseWriter, r *http.Request, u *store.User) {
	writeJSON(w, http.StatusOK, u)
}

// patchMe updates the signed-in user's own settings; omitted fields are left alone.
func (s *Server) patchMe(w http.ResponseWriter, r *http.Request, u *store.User) {
	var body struct {
		NotifyPref  *string `json:"notify_pref"`
		DisplayName *string `json:"display_name"` // empty resets to the Google name
	}
	if err := readJSON(r, &body); err != nil {
		httpError(w, http.StatusBadRequest, "bad body")
		return
	}
	if p := body.NotifyPref; p != nil {
		switch *p {
		case "mine", "all", "none":
		default:
			httpError(w, http.StatusBadRequest, "notify_pref must be mine, all or none")
			return
		}
		if err := s.St.SetNotifyPref(r.Context(), u.Email, *p); err != nil {
			internalError(w, err)
			return
		}
		u.NotifyPref = *p
	}
	if n := body.DisplayName; n != nil {
		name := strings.Join(strings.Fields(*n), " ")
		if len([]rune(name)) > 40 {
			httpError(w, http.StatusBadRequest, "name is too long (max 40 characters)")
			return
		}
		if err := s.St.SetDisplayName(r.Context(), u.Email, name); err != nil {
			internalError(w, err)
			return
		}
		u.DisplayName = name
	}
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) listDevices(w http.ResponseWriter, r *http.Request, u *store.User) {
	ds, err := s.St.ListDevices(r.Context())
	if err != nil {
		internalError(w, err)
		return
	}
	for i := range ds {
		ds[i].Online = s.Hub.Online(ds[i].ID)
	}
	if ds == nil {
		ds = []store.Device{}
	}
	writeJSON(w, http.StatusOK, ds)
}

func (s *Server) listGroups(w http.ResponseWriter, r *http.Request, u *store.User) {
	gs, err := s.St.ListGroups(r.Context())
	if err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, gs)
}

func (s *Server) listTypes(w http.ResponseWriter, r *http.Request, u *store.User) {
	ts, err := s.St.ListTypes(r.Context())
	if err != nil {
		internalError(w, err)
		return
	}
	if ts == nil {
		ts = []store.Type{}
	}
	writeJSON(w, http.StatusOK, ts)
}

func (s *Server) getPresets(w http.ResponseWriter, r *http.Request, u *store.User) {
	p, err := s.St.Presets(r.Context())
	if err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) listAlerts(w http.ResponseWriter, r *http.Request, u *store.User) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	as, err := s.St.ListAlerts(r.Context(), limit)
	if err != nil {
		internalError(w, err)
		return
	}
	if as == nil {
		as = []*store.Alert{}
	}
	writeJSON(w, http.StatusOK, as)
}

func (s *Server) createAlert(w http.ResponseWriter, r *http.Request, u *store.User) {
	var body struct {
		Device  string `json:"device"` // PC name, group name or "all"
		Group   int64  `json:"group"`  // group id; overrides device
		Type    string `json:"type"`   // type id or name
		Message string `json:"message"`
	}
	if err := readJSON(r, &body); err != nil {
		httpError(w, http.StatusBadRequest, "bad body")
		return
	}
	devices, err := s.St.ListDevices(r.Context())
	if err != nil {
		internalError(w, err)
		return
	}
	if len(devices) == 0 {
		httpError(w, http.StatusNotFound, "no PCs are paired yet")
		return
	}
	var targets []store.Device
	if body.Group != 0 {
		targets, err = s.St.GroupDevices(r.Context(), body.Group)
		if errors.Is(err, store.ErrNotFound) {
			httpError(w, http.StatusNotFound, "unknown group")
			return
		}
		if err != nil {
			internalError(w, err)
			return
		}
		if len(targets) == 0 {
			httpError(w, http.StatusBadRequest, "that group has no PCs")
			return
		}
	} else if targets, err = s.resolveDevices(r.Context(), body.Device); err != nil {
		httpError(w, http.StatusNotFound, "unknown PC: "+err.Error())
		return
	}
	typeID, err := s.resolveType(r.Context(), body.Type)
	if err != nil {
		httpError(w, http.StatusNotFound, "unknown attention type")
		return
	}
	res, err := s.raise(r.Context(), targets, typeID, store.Requester{Email: u.Email, Name: requesterName(u)}, body.Message)
	if err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// requesterName is how a user appears on the popup: their chosen name, else their first name.
func requesterName(u *store.User) string {
	if u.DisplayName != "" {
		return u.DisplayName
	}
	if u.Name != "" {
		return firstName(u.Name)
	}
	return firstName(u.Email)
}

func firstName(n string) string {
	if at := strings.IndexByte(n, '@'); at > 0 {
		return n[:at]
	}
	if f := strings.Fields(n); len(f) > 0 {
		return f[0]
	}
	return n
}

func (s *Server) cancelAlert(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, ok := pathID(r)
	if !ok {
		httpError(w, http.StatusBadRequest, "bad id")
		return
	}
	err := s.Hub.Cancel(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		httpError(w, http.StatusNotFound, "no such alert")
	case errors.Is(err, store.ErrNotOpen):
		httpError(w, http.StatusConflict, "alert already resolved")
	case err != nil:
		internalError(w, err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// events streams alert and device changes to the PWA (Server-Sent Events).
func (s *Server) events(w http.ResponseWriter, r *http.Request, u *store.User) {
	fl, ok := w.(http.Flusher)
	if !ok {
		httpError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	ch := s.Hub.Subscribe()
	defer s.Hub.Unsubscribe(ch)
	fmt.Fprint(w, "retry: 3000\n\n")
	fl.Flush()
	// Keepalive under Cloudflare's 100 s idle timeout.
	tick := time.NewTicker(25 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case b := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		case <-tick.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		}
	}
}

func (s *Server) pushSubscribe(w http.ResponseWriter, r *http.Request, u *store.User) {
	var body struct {
		Endpoint string `json:"endpoint"`
		Keys     struct {
			P256dh string `json:"p256dh"`
			Auth   string `json:"auth"`
		} `json:"keys"`
	}
	if err := readJSON(r, &body); err != nil || !strings.HasPrefix(body.Endpoint, "https://") || body.Keys.P256dh == "" || body.Keys.Auth == "" {
		httpError(w, http.StatusBadRequest, "bad subscription")
		return
	}
	if err := s.St.AddPushSub(r.Context(), u.Email, store.PushSub{Endpoint: body.Endpoint, P256dh: body.Keys.P256dh, Auth: body.Keys.Auth}); err != nil {
		internalError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) pushUnsubscribe(w http.ResponseWriter, r *http.Request, u *store.User) {
	var body struct {
		Endpoint string `json:"endpoint"`
	}
	if err := readJSON(r, &body); err != nil {
		httpError(w, http.StatusBadRequest, "bad body")
		return
	}
	_ = s.St.DeletePushSub(r.Context(), body.Endpoint)
	w.WriteHeader(http.StatusNoContent)
}

// serveMedia serves a type's GIF or sound to signed-in users (previews) and PCs (cache sync).
func (s *Server) serveMedia(w http.ResponseWriter, r *http.Request) {
	if s.sessionUser(r) == nil && s.deviceFromRequest(r) == nil {
		httpError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	t, err := s.St.GetType(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var path, mime string
	switch r.PathValue("kind") {
	case "gif":
		path, mime = s.Media.GIFPath(id), "image/gif"
	case "sound":
		path, mime = s.Media.SoundPath(id), t.SoundMime
	default:
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("ETag", `"`+t.Hash+`"`)
	if r.URL.Query().Get("h") == t.Hash {
		w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "private, no-cache")
	}
	http.ServeFile(w, r, path)
}
