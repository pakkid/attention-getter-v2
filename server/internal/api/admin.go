package api

import (
	"net/http"
	"os"
	"strings"

	"attention-getter/server/internal/media"
	"attention-getter/server/internal/store"
)

func (s *Server) adminListUsers(w http.ResponseWriter, r *http.Request, u *store.User) {
	us, err := s.St.ListUsers(r.Context())
	if err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, us)
}

func (s *Server) adminUpsertUser(w http.ResponseWriter, r *http.Request, u *store.User) {
	var body struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if err := readJSON(r, &body); err != nil || !strings.Contains(body.Email, "@") {
		httpError(w, http.StatusBadRequest, "valid email required")
		return
	}
	if body.Role != "admin" {
		body.Role = "user"
	}
	if strings.EqualFold(body.Email, u.Email) && body.Role != "admin" {
		httpError(w, http.StatusBadRequest, "you can't remove your own admin role")
		return
	}
	if err := s.St.UpsertUser(r.Context(), strings.TrimSpace(body.Email), body.Role); err != nil {
		internalError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) adminDeleteUser(w http.ResponseWriter, r *http.Request, u *store.User) {
	email := r.PathValue("email")
	if strings.EqualFold(email, u.Email) {
		httpError(w, http.StatusBadRequest, "you can't remove yourself")
		return
	}
	if err := s.St.DeleteUser(r.Context(), email); err != nil {
		internalError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// adminSaveType creates (POST) or updates (PUT) a type from a multipart form with
// fields name, gif and sound. On update, omitted files are kept.
func (s *Server) adminSaveType(w http.ResponseWriter, r *http.Request, u *store.User) {
	r.Body = http.MaxBytesReader(w, r.Body, media.MaxGIF+media.MaxSound+(1<<20))
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		httpError(w, http.StatusBadRequest, "upload too large or malformed")
		return
	}
	defer r.MultipartForm.RemoveAll()
	name := strings.TrimSpace(r.FormValue("name"))

	var existing *store.Type
	if r.Method == http.MethodPut {
		id, ok := pathID(r)
		if !ok {
			httpError(w, http.StatusBadRequest, "bad id")
			return
		}
		t, err := s.St.GetType(r.Context(), id)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		existing = t
		if name == "" {
			name = t.Name
		}
	}
	if name == "" || len(name) > 40 {
		httpError(w, http.StatusBadRequest, "name required (max 40 chars)")
		return
	}

	var gif, sound []byte
	var soundMime string
	if f, _, err := r.FormFile("gif"); err == nil {
		gif, err = media.ReadGIF(f)
		f.Close()
		if err != nil {
			httpError(w, http.StatusBadRequest, "gif: "+err.Error())
			return
		}
	}
	if f, _, err := r.FormFile("sound"); err == nil {
		sound, soundMime, err = media.ReadSound(f)
		f.Close()
		if err != nil {
			httpError(w, http.StatusBadRequest, "sound: "+err.Error())
			return
		}
	}

	if existing == nil {
		if gif == nil || sound == nil {
			httpError(w, http.StatusBadRequest, "both a GIF and a sound are required")
			return
		}
		id, err := s.St.CreateType(r.Context(), name, soundMime, media.Hash(gif, sound))
		if err != nil {
			httpError(w, http.StatusConflict, "a type with that name already exists")
			return
		}
		if err := s.writeMedia(id, gif, sound); err != nil {
			_ = s.St.DeleteType(r.Context(), id)
			internalError(w, err)
			return
		}
		s.Hub.ManifestChanged()
		writeJSON(w, http.StatusOK, map[string]int64{"id": id})
		return
	}

	var err error
	if gif == nil {
		if gif, err = os.ReadFile(s.Media.GIFPath(existing.ID)); err != nil {
			internalError(w, err)
			return
		}
	}
	if sound == nil {
		if sound, err = os.ReadFile(s.Media.SoundPath(existing.ID)); err != nil {
			internalError(w, err)
			return
		}
		soundMime = existing.SoundMime
	}
	if err := s.writeMedia(existing.ID, gif, sound); err != nil {
		internalError(w, err)
		return
	}
	existing.Name, existing.SoundMime, existing.Hash = name, soundMime, media.Hash(gif, sound)
	if err := s.St.UpdateType(r.Context(), existing); err != nil {
		httpError(w, http.StatusConflict, "a type with that name already exists")
		return
	}
	s.Hub.ManifestChanged()
	writeJSON(w, http.StatusOK, map[string]int64{"id": existing.ID})
}

func (s *Server) writeMedia(id int64, gif, sound []byte) error {
	if err := media.WriteFile(s.Media.GIFPath(id), gif); err != nil {
		return err
	}
	return media.WriteFile(s.Media.SoundPath(id), sound)
}

func (s *Server) adminDeleteType(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, ok := pathID(r)
	if !ok {
		httpError(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := s.St.DeleteType(r.Context(), id); err != nil {
		internalError(w, err)
		return
	}
	_ = os.Remove(s.Media.GIFPath(id))
	_ = os.Remove(s.Media.SoundPath(id))
	s.Hub.ManifestChanged()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) adminListKeys(w http.ResponseWriter, r *http.Request, u *store.User) {
	ks, err := s.St.ListAPIKeys(r.Context())
	if err != nil {
		internalError(w, err)
		return
	}
	if ks == nil {
		ks = []store.APIKey{}
	}
	writeJSON(w, http.StatusOK, ks)
}

func (s *Server) adminCreateKey(w http.ResponseWriter, r *http.Request, u *store.User) {
	var body struct {
		Name         string `json:"name"`
		PinnedType   *int64 `json:"pinned_type"`
		PinnedDevice *int64 `json:"pinned_device"`
	}
	if err := readJSON(r, &body); err != nil || strings.TrimSpace(body.Name) == "" {
		httpError(w, http.StatusBadRequest, "name required")
		return
	}
	key, err := s.St.CreateAPIKey(r.Context(), strings.TrimSpace(body.Name), u.Email, body.PinnedType, body.PinnedDevice)
	if err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"key": key})
}

func (s *Server) adminDeleteKey(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, ok := pathID(r)
	if !ok {
		httpError(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := s.St.DeleteAPIKey(r.Context(), id); err != nil {
		internalError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) adminPairingCode(w http.ResponseWriter, r *http.Request, u *store.User) {
	code, err := s.St.CreatePairingCode(r.Context(), u.Email)
	if err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"code": code})
}

func (s *Server) adminDeleteDevice(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, ok := pathID(r)
	if !ok {
		httpError(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := s.St.DeleteDevice(r.Context(), id); err != nil {
		internalError(w, err)
		return
	}
	s.Hub.Kick(id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) adminSetPresets(w http.ResponseWriter, r *http.Request, u *store.User) {
	var presets []string
	if err := readJSON(r, &presets); err != nil {
		httpError(w, http.StatusBadRequest, "expected a JSON array of strings")
		return
	}
	var clean []string
	for _, p := range presets {
		if p = strings.TrimSpace(p); p != "" {
			clean = append(clean, p)
		}
	}
	if len(clean) > 9 {
		httpError(w, http.StatusBadRequest, "at most 9 presets (keys 1-9)")
		return
	}
	if err := s.St.SetPresets(r.Context(), clean); err != nil {
		internalError(w, err)
		return
	}
	s.Hub.ManifestChanged()
	w.WriteHeader(http.StatusNoContent)
}
