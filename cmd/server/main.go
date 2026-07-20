package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"vibedock/internal/app"
	dockerruntime "vibedock/internal/docker"
	"vibedock/internal/model"
	"vibedock/internal/secrets"
	"vibedock/internal/store"
)

//go:embed web/*
var embeddedWeb embed.FS

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	statePath := env("VIBEDOCK_STATE", "/data/workspaces.json")
	templatePath := env("VIBEDOCK_TEMPLATES", "/config/templates.json")
	listenAddr := env("VIBEDOCK_LISTEN", ":8787")

	st, err := store.New(statePath)
	must(err)
	keyring, err := secrets.New(env("VIBEDOCK_CREDENTIAL_KEY_FILE", "/data/credential.key"), os.Getenv("VIBEDOCK_CREDENTIAL_KEY"))
	must(err)
	templates, err := loadTemplates(templatePath)
	must(err)
	web, err := fs.Sub(embeddedWeb, "web")
	must(err)

	adminUser := env("VIBEDOCK_ADMIN_USER", "vibe")
	adminPassword := strings.TrimSpace(os.Getenv("VIBEDOCK_ADMIN_PASSWORD"))
	runtime := dockerruntime.New(env("VIBEDOCK_DOCKER_COMMAND", "docker"), env("VIBEDOCK_DOCKER_NETWORK", "vibedock"))
	server := app.NewServerWithAuth(st, runtime, runtime, templates, web, logger, keyring, app.AccountAuthOptions{
		Enabled: true, ControlPlaneAuthEnabled: adminPassword != "",
		TTL:           parseDurationEnv("VIBEDOCK_LOGIN_TTL", 15*time.Minute),
		MaxConcurrent: parseIntEnv("VIBEDOCK_MAX_LOGIN_HELPERS", 3),
	})
	server.Reconcile(context.Background())

	handler := server.Handler()
	if adminPassword != "" {
		handler = basicAuth(handler, adminUser, adminPassword)
	} else {
		logger.Warn("control plane authentication is disabled; use only on a trusted network")
	}

	httpServer := &http.Server{
		Addr:              listenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	shutdownContext, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	go func() {
		<-shutdownContext.Done()
		cleanupContext, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		server.Close(cleanupContext)
		_ = httpServer.Shutdown(cleanupContext)
	}()

	logger.Info("VibeDock listening", "address", listenAddr, "templates", len(templates))
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func loadTemplates(path string) ([]model.Template, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read templates %s: %w", path, err)
	}
	var templates []model.Template
	if err := json.Unmarshal(data, &templates); err != nil {
		return nil, fmt.Errorf("decode templates: %w", err)
	}
	if len(templates) == 0 {
		return nil, fmt.Errorf("templates file is empty")
	}
	for i := range templates {
		templates[i].ID = strings.TrimSpace(templates[i].ID)
		templates[i].Image = strings.TrimSpace(templates[i].Image)
		if templates[i].ID == "" || templates[i].Image == "" {
			return nil, fmt.Errorf("template %d requires id and image", i)
		}
	}
	return templates, nil
}

func env(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func parseDurationEnv(key string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func parseIntEnv(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func basicAuth(next http.Handler, username, password string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providedUser, providedPassword, ok := r.BasicAuth()
		userOK := subtle.ConstantTimeCompare([]byte(providedUser), []byte(username)) == 1
		passwordOK := subtle.ConstantTimeCompare([]byte(providedPassword), []byte(password)) == 1
		if !ok || !userOK || !passwordOK {
			w.Header().Set("WWW-Authenticate", `Basic realm="VibeDock", charset="UTF-8"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
