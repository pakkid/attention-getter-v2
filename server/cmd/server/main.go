// Command server runs the attention-getter server: PWA, trigger API and PC WebSocket hub.
package main

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	attention "attention-getter/server"
	"attention-getter/server/internal/api"
	"attention-getter/server/internal/auth"
	"attention-getter/server/internal/hub"
	"attention-getter/server/internal/media"
	"attention-getter/server/internal/push"
	"attention-getter/server/internal/store"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	dataDir := env("DATA_DIR", "/data")
	addr := env("LISTEN_ADDR", ":8080")
	publicURL := strings.TrimRight(env("PUBLIC_URL", "http://localhost:8080"), "/")
	clientID := os.Getenv("GOOGLE_CLIENT_ID")
	devLogin := os.Getenv("DEV_LOGIN") == "1"

	mediaDir := filepath.Join(dataDir, "media")
	if err := os.MkdirAll(mediaDir, 0o755); err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(dataDir, "app.db"))
	if err != nil {
		return err
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	for _, e := range strings.Split(os.Getenv("ADMIN_EMAILS"), ",") {
		if e = strings.TrimSpace(e); e != "" {
			if err := st.UpsertUser(ctx, e, "admin"); err != nil {
				return err
			}
		}
	}
	if clientID == "" && !devLogin {
		slog.Warn("GOOGLE_CLIENT_ID is not set; nobody can sign in")
	}
	if devLogin {
		slog.Warn("DEV_LOGIN=1: anyone can sign in as any allowlisted email. Never expose this publicly.")
	}

	keys, err := push.LoadOrCreateKeys(filepath.Join(dataDir, "vapid.json"))
	if err != nil {
		return err
	}
	ps := push.New(keys, env("VAPID_SUBJECT", publicURL), st)

	static, err := fs.Sub(attention.Web, "web")
	if err != nil {
		return err
	}
	srv := &api.Server{
		St:            st,
		Hub:           hub.New(st, ps),
		Push:          ps,
		Google:        auth.NewGoogleVerifier(clientID),
		Media:         media.Dir(mediaDir),
		SecureCookies: strings.HasPrefix(publicURL, "https://"),
		DevLogin:      devLogin,
		Static:        static,
	}
	hs := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hs.Shutdown(sctx)
	}()
	slog.Info("listening", "addr", addr, "public_url", publicURL)
	if err := hs.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
