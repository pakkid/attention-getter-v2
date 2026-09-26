// Package api wires HTTP routes for the PWA, the trigger endpoint and PC clients.
package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"attention-getter/server/internal/auth"
	"attention-getter/server/internal/hub"
	"attention-getter/server/internal/media"
	"attention-getter/server/internal/push"
	"attention-getter/server/internal/store"
)

const sessionCookie = "ag_session"

type Server struct {
	St            *store.Store
	Hub           *hub.Hub
	Push          *push.Service
	Google        *auth.GoogleVerifier
	Media         media.Dir
	SecureCookies bool
	DevLogin      bool // enables /auth/dev for local testing without Google
	Static        fs.FS
}

func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()

	m.HandleFunc("GET /api/config", s.config)
	m.HandleFunc("POST /auth/google", s.loginGoogle)
	m.HandleFunc("POST /auth/dev", s.loginDev)
	m.HandleFunc("POST /auth/logout", s.logout)

	m.HandleFunc("GET /api/me", s.user(s.getMe))
	m.HandleFunc("PATCH /api/me", s.user(s.patchMe))
	m.HandleFunc("GET /api/devices", s.user(s.listDevices))
	m.HandleFunc("GET /api/groups", s.user(s.listGroups))
	m.HandleFunc("GET /api/types", s.user(s.listTypes))
	m.HandleFunc("GET /api/alerts", s.user(s.listAlerts))
	m.HandleFunc("POST /api/alerts", s.user(s.createAlert))
	m.HandleFunc("POST /api/alerts/{id}/cancel", s.user(s.cancelAlert))
	m.HandleFunc("GET /api/events", s.user(s.events))
	m.HandleFunc("POST /api/push/subscribe", s.user(s.pushSubscribe))
	m.HandleFunc("POST /api/push/unsubscribe", s.user(s.pushUnsubscribe))
	m.HandleFunc("GET /media/{id}/{kind}", s.serveMedia)

	m.HandleFunc("GET /api/admin/users", s.admin(s.adminListUsers))
	m.HandleFunc("POST /api/admin/users", s.admin(s.adminUpsertUser))
	m.HandleFunc("DELETE /api/admin/users/{email}", s.admin(s.adminDeleteUser))
	m.HandleFunc("POST /api/admin/types", s.admin(s.adminSaveType))
	m.HandleFunc("PUT /api/admin/types/{id}", s.admin(s.adminSaveType))
	m.HandleFunc("DELETE /api/admin/types/{id}", s.admin(s.adminDeleteType))
	m.HandleFunc("GET /api/admin/keys", s.admin(s.adminListKeys))
	m.HandleFunc("POST /api/admin/keys", s.admin(s.adminCreateKey))
	m.HandleFunc("DELETE /api/admin/keys/{id}", s.admin(s.adminDeleteKey))
	m.HandleFunc("POST /api/admin/pairing", s.admin(s.adminPairingCode))
	m.HandleFunc("PATCH /api/admin/devices/{id}", s.admin(s.adminRenameDevice))
	m.HandleFunc("DELETE /api/admin/devices/{id}", s.admin(s.adminDeleteDevice))
	m.HandleFunc("POST /api/admin/groups", s.admin(s.adminSaveGroup))
	m.HandleFunc("PUT /api/admin/groups/{id}", s.admin(s.adminSaveGroup))
	m.HandleFunc("DELETE /api/admin/groups/{id}", s.admin(s.adminDeleteGroup))
	m.HandleFunc("GET /api/admin/settings", s.admin(s.adminGetSettings))
	m.HandleFunc("PUT /api/admin/settings", s.admin(s.adminSaveSettings))
	m.HandleFunc("GET /api/presets", s.user(s.getPresets))
	m.HandleFunc("PUT /api/admin/presets", s.admin(s.adminSetPresets))

	m.HandleFunc("POST /api/trigger", s.trigger)

	m.HandleFunc("POST /api/device/pair", s.devicePair)
	m.HandleFunc("GET /api/device/ws", s.device(s.deviceWS))
	m.HandleFunc("GET /api/device/manifest", s.device(s.deviceManifest))

	m.Handle("GET /", staticHandler(s.Static))
	return securityHeaders(m)
}

// ---- middleware ----

type userHandler func(w http.ResponseWriter, r *http.Request, u *store.User)

func (s *Server) sessionUser(r *http.Request) *store.User {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil
	}
	u, err := s.St.SessionUser(r.Context(), c.Value)
	if err != nil {
		return nil
	}
	return u
}

// user requires a signed-in user. State-changing requests must carry X-AG-CSRF, which a
// cross-site form cannot send, so SameSite=Lax cookies plus this header block CSRF.
func (s *Server) user(h userHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := s.sessionUser(r)
		if u == nil {
			httpError(w, http.StatusUnauthorized, "sign in required")
			return
		}
		if r.Method != http.MethodGet && r.Header.Get("X-AG-CSRF") == "" {
			httpError(w, http.StatusForbidden, "missing CSRF header")
			return
		}
		h(w, r, u)
	}
}

func (s *Server) admin(h userHandler) http.HandlerFunc {
	return s.user(func(w http.ResponseWriter, r *http.Request, u *store.User) {
		if !u.IsAdmin() {
			httpError(w, http.StatusForbidden, "admins only")
			return
		}
		h(w, r, u)
	})
}

type deviceHandler func(w http.ResponseWriter, r *http.Request, d *store.Device)

func (s *Server) deviceFromRequest(r *http.Request) *store.Device {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || tok == "" {
		return nil
	}
	d, err := s.St.DeviceByToken(r.Context(), tok)
	if err != nil {
		return nil
	}
	return d
}

func (s *Server) device(h deviceHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		d := s.deviceFromRequest(r)
		if d == nil {
			httpError(w, http.StatusUnauthorized, "invalid device token")
			return
		}
		h(w, r, d)
	}
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		// Google Identity Services opens a popup and needs same-origin-allow-popups.
		h.Set("Cross-Origin-Opener-Policy", "same-origin-allow-popups")
		next.ServeHTTP(w, r)
	})
}

func staticHandler(root fs.FS) http.Handler {
	files := http.FileServerFS(root)
	// Content-hash ETags: embedded files have no modtime, so this is what lets browsers revalidate cheaply.
	etags := map[string]string{}
	_ = fs.WalkDir(root, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(root, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		etags["/"+p] = `"` + hex.EncodeToString(sum[:8]) + `"`
		return nil
	})
	etags["/"] = etags["/index.html"]
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Always revalidate (unchanged files get a 304). Without an explicit header Cloudflare adds a
		// 4-hour browser TTL, and a deploy doesn't show up until that runs out.
		w.Header().Set("Cache-Control", "no-cache")
		if tag, ok := etags[r.URL.Path]; ok {
			w.Header().Set("ETag", tag)
		}
		files.ServeHTTP(w, r)
	})
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func internalError(w http.ResponseWriter, err error) {
	slog.Error("request failed", "err", err)
	httpError(w, http.StatusInternalServerError, "internal error")
}

func readJSON(r *http.Request, v any) error {
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 64<<10)).Decode(v)
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil
}

// resolveDevices maps a "pc" parameter to target PCs: a PC name, a group name, "all", or empty
// when only one PC exists.
func (s *Server) resolveDevices(ctx context.Context, pc string) ([]store.Device, error) {
	pc = strings.TrimSpace(pc)
	if pc != "" && !strings.EqualFold(pc, "all") {
		d, err := s.St.DeviceByName(ctx, pc)
		if err == nil {
			return []store.Device{*d}, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
		g, ds, err := s.St.GroupByName(ctx, pc)
		if err != nil {
			return nil, err
		}
		if len(ds) == 0 {
			return nil, errors.New("group " + g.Name + " has no PCs")
		}
		return ds, nil
	}
	all, err := s.St.ListDevices(ctx)
	if err != nil {
		return nil, err
	}
	if pc == "" && len(all) > 1 {
		return nil, errors.New("several PCs are paired; pass pc=<name> or pc=all")
	}
	if len(all) == 0 {
		return nil, store.ErrNotFound
	}
	return all, nil
}

// resolveType maps a "type" parameter (name or id) to a type. Empty picks "default", then the first type.
func (s *Server) resolveType(ctx context.Context, t string) (*int64, error) {
	t = strings.TrimSpace(t)
	if t != "" {
		if id, err := strconv.ParseInt(t, 10, 64); err == nil {
			if _, err := s.St.GetType(ctx, id); err != nil {
				return nil, err
			}
			return &id, nil
		}
		tp, err := s.St.TypeByName(ctx, t)
		if err != nil {
			return nil, err
		}
		return &tp.ID, nil
	}
	if tp, err := s.St.TypeByName(ctx, "default"); err == nil {
		return &tp.ID, nil
	}
	types, err := s.St.ListTypes(ctx)
	if err != nil || len(types) == 0 {
		return nil, err // no types yet: popup shows without media
	}
	return &types[0].ID, nil
}

type triggerResult struct {
	AlertID int64  `json:"alert_id"`
	Device  string `json:"device"`
	Merged  bool   `json:"merged"`
	Online  bool   `json:"online"`
	Missed  bool   `json:"missed"` // PC offline and offline delivery is off: not sent
}

func (s *Server) raise(ctx context.Context, devices []store.Device, typeID *int64, r store.Requester, message string) ([]triggerResult, error) {
	message = strings.TrimSpace(message)
	if len([]rune(message)) > 200 {
		message = string([]rune(message)[:200])
	}
	var out []triggerResult
	for _, d := range devices {
		a, err := s.Hub.Trigger(ctx, d.ID, typeID, r, message)
		if err != nil {
			return nil, err
		}
		out = append(out, triggerResult{
			AlertID: a.ID, Device: d.Name, Merged: len(a.Requests) > 1, Online: s.Hub.Online(d.ID),
			Missed: a.Status == store.StatusMissed,
		})
	}
	return out, nil
}

// validName checks a PC or group name: 1-40 chars, not "all", not used by another PC or group.
// It returns the trimmed name, or an HTTP status and message.
func (s *Server) validName(ctx context.Context, name string, exceptDevice, exceptGroup int64) (string, int, string) {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > 40 || strings.EqualFold(name, "all") {
		return "", http.StatusBadRequest, "name must be 1-40 characters and not \"all\""
	}
	taken, err := s.St.NameTaken(ctx, name, exceptDevice, exceptGroup)
	if err != nil {
		return "", http.StatusInternalServerError, "internal error"
	}
	if taken {
		return "", http.StatusConflict, "a PC or group is already called " + name
	}
	return name, 0, ""
}

var sessionTTL = 180 * 24 * time.Hour
