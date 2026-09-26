package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"

	"attention-getter/server/internal/store"
)

// trigger is the Alexa / automation endpoint: POST /api/trigger?key=…&type=…&pc=…&message=…
func (s *Server) trigger(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	k, err := s.St.APIKeyBySecret(r.Context(), q.Get("key"))
	if err != nil {
		httpError(w, http.StatusUnauthorized, "invalid key")
		return
	}
	var targets []store.Device
	if k.PinnedDevice != nil {
		ds, err := s.St.ListDevices(r.Context())
		if err != nil {
			internalError(w, err)
			return
		}
		for _, d := range ds {
			if d.ID == *k.PinnedDevice {
				targets = append(targets, d)
			}
		}
	} else if targets, err = s.resolveDevices(r.Context(), q.Get("pc")); err != nil {
		httpError(w, http.StatusNotFound, "unknown pc: "+err.Error())
		return
	}
	if len(targets) == 0 {
		httpError(w, http.StatusNotFound, "no PCs paired")
		return
	}
	typeID := k.PinnedType
	if typeID == nil {
		if typeID, err = s.resolveType(r.Context(), q.Get("type")); err != nil {
			httpError(w, http.StatusNotFound, "unknown type")
			return
		}
	}
	res, err := s.raise(r.Context(), targets, typeID, store.Requester{Email: k.OwnerEmail, APIKeyID: k.ID, Name: k.Name}, q.Get("message"))
	if err != nil {
		internalError(w, err)
		return
	}
	var missed []string
	for _, a := range res {
		if a.Missed {
			missed = append(missed, a.Device)
		}
	}
	if len(missed) == len(res) {
		// Nothing was sent: every target PC is offline and offline delivery is off.
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": strings.Join(missed, ", ") + " offline", "alerts": res})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "alerts": res})
}

func (s *Server) devicePair(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code     string `json:"code"`
		Name     string `json:"name"`
		Replaces string `json:"replaces"` // the client's old token when re-pairing (1.1.0+)
	}
	if err := readJSON(r, &body); err != nil {
		httpError(w, http.StatusBadRequest, "bad body")
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" || len(body.Name) > 40 || strings.EqualFold(body.Name, "all") {
		httpError(w, http.StatusBadRequest, "invalid PC name")
		return
	}
	// Pairing under an existing name (any case) adds an install to that PC, e.g. the other OS of a
	// dual-boot PC. A group name can't be used.
	if d, err := s.St.DeviceByName(r.Context(), body.Name); err == nil {
		body.Name = d.Name
	} else if _, _, err := s.St.GroupByName(r.Context(), body.Name); err == nil {
		httpError(w, http.StatusConflict, "a group is already called "+body.Name+"; pick another PC name")
		return
	}
	p, err := s.St.PairDevice(r.Context(), body.Code, body.Name, body.Replaces)
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusForbidden, "pairing code invalid or expired")
		return
	}
	if err != nil {
		internalError(w, err)
		return
	}
	if p.ReplacedInstall != 0 {
		s.Hub.KickInstall(p.ReplacedDevice, p.ReplacedInstall) // the old token no longer works
	}
	s.Hub.DevicesChanged()
	writeJSON(w, http.StatusOK, map[string]any{"id": p.DeviceID, "token": p.Token, "name": body.Name})
}

type manifest struct {
	Types   []manifestType `json:"types"`
	Presets []string       `json:"presets"`
}

type manifestType struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Hash string `json:"hash"`
}

func (s *Server) deviceManifest(w http.ResponseWriter, r *http.Request, d *store.Device) {
	ts, err := s.St.ListTypes(r.Context())
	if err != nil {
		internalError(w, err)
		return
	}
	presets, err := s.St.Presets(r.Context())
	if err != nil {
		internalError(w, err)
		return
	}
	m := manifest{Types: []manifestType{}, Presets: presets}
	for _, t := range ts {
		m.Types = append(m.Types, manifestType{ID: t.ID, Name: t.Name, Hash: t.Hash})
	}
	b, _ := json.Marshal(m)
	sum := sha256.Sum256(b)
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

// deviceWS keeps a PC connected so alerts arrive instantly.
func (s *Server) deviceWS(w http.ResponseWriter, r *http.Request, d *store.Device) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	c.SetReadLimit(64 << 10)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	agent := r.Header.Get("User-Agent")
	_ = s.St.SetInstallAgent(ctx, d.InstallID, agent)
	_ = s.St.TouchDevice(ctx, d)
	conn := s.Hub.Attach(ctx, d.ID, d.InstallID)
	defer s.Hub.Detach(d.ID, d.InstallID, conn)
	slog.Info("pc connected", "pc", d.Name, "install", d.InstallID, "agent", agent)
	defer slog.Info("pc disconnected", "pc", d.Name, "install", d.InstallID)

	go func() {
		defer cancel()
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			if err := s.Hub.HandleDeviceMessage(ctx, d.ID, d.InstallID, data); err != nil {
				slog.Warn("device message", "pc", d.Name, "err", err)
			}
		}
	}()

	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			c.Close(websocket.StatusGoingAway, "")
			return
		case <-conn.Done:
			c.Close(websocket.StatusPolicyViolation, "replaced")
			return
		case b := <-conn.Send:
			wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
			err := c.Write(wctx, websocket.MessageText, b)
			wcancel()
			if err != nil {
				return
			}
		case <-ping.C:
			pctx, pcancel := context.WithTimeout(ctx, 10*time.Second)
			err := c.Ping(pctx)
			pcancel()
			if err != nil {
				return
			}
			_ = s.St.TouchDevice(ctx, d)
		}
	}
}
