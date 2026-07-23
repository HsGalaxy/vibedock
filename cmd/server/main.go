package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"vibedock/internal/app"
	"vibedock/internal/controlauth"
	dockerruntime "vibedock/internal/docker"
	"vibedock/internal/model"
	"vibedock/internal/secrets"
	"vibedock/internal/store"
)

//go:embed web/*
var embeddedWeb embed.FS

type publicRuntimeConfig struct {
	PreviewOrigin string `json:"preview_origin"`
	PreviewPort   int    `json:"preview_port"`
}

type httpEndpoint struct {
	name   string
	server *http.Server
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	statePath := env("VIBEDOCK_STATE", "/data/workspaces.json")
	templatePath := env("VIBEDOCK_TEMPLATES", "/config/templates.json")
	listenAddr := env("VIBEDOCK_LISTEN", "127.0.0.1:8787")
	previewListenAddr := env("VIBEDOCK_PREVIEW_LISTEN", "127.0.0.1:8788")
	previewOrigin, err := normalizePreviewOrigin(os.Getenv("VIBEDOCK_PREVIEW_ORIGIN"))
	must(err)
	previewPublicPort, err := parsePortEnv("VIBEDOCK_PREVIEW_PUBLIC_PORT", 8788)
	must(err)

	st, err := store.New(statePath)
	must(err)
	keyring, err := secrets.New(env("VIBEDOCK_CREDENTIAL_KEY_FILE", "/data/credential.key"), os.Getenv("VIBEDOCK_CREDENTIAL_KEY"))
	must(err)
	templates, err := loadTemplates(templatePath)
	must(err)
	web, err := fs.Sub(embeddedWeb, "web")
	must(err)

	controlAuth, err := controlauth.New(controlauth.Options{
		CredentialFile:     env("VIBEDOCK_AUTH_FILE", "/data/admin.json"),
		BootstrapTokenFile: env("VIBEDOCK_BOOTSTRAP_TOKEN_FILE", "/data/bootstrap.token"),
		SessionTTL:         parseDurationEnv("VIBEDOCK_SESSION_TTL", 12*time.Hour),
		TrustProxyHeaders:  parseBoolEnv("VIBEDOCK_TRUST_PROXY_HEADERS", false),
	})
	must(err)
	runtime := dockerruntime.New(env("VIBEDOCK_DOCKER_COMMAND", "docker"), env("VIBEDOCK_DOCKER_NETWORK", "vibedock"))
	server := app.NewServerWithAuth(st, runtime, runtime, templates, web, logger, keyring, app.AccountAuthOptions{
		Enabled: true, ControlPlaneAuthEnabled: true,
		TTL:           parseDurationEnv("VIBEDOCK_LOGIN_TTL", 15*time.Minute),
		MaxConcurrent: parseIntEnv("VIBEDOCK_MAX_LOGIN_HELPERS", 3),
	})
	server.Reconcile(context.Background())

	applicationHandler := server.Handler()
	runtimeConfig := publicRuntimeConfig{PreviewOrigin: previewOrigin, PreviewPort: previewPublicPort}
	controlServer := &http.Server{
		Addr:              listenAddr,
		Handler:           controlAuth.Handler(controlApplicationHandler(applicationHandler, runtimeConfig)),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	previewServer := &http.Server{
		Addr:              previewListenAddr,
		Handler:           controlAuth.SessionMiddleware(previewApplicationHandler(applicationHandler)),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	shutdownContext, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()

	logger.Info("VibeDock starting", "control_address", listenAddr, "preview_address", previewListenAddr, "templates", len(templates), "administrator_initialized", controlAuth.Initialized())
	serveErr := serveHTTPServers(shutdownContext, 20*time.Second,
		httpEndpoint{name: "control", server: controlServer},
		httpEndpoint{name: "preview", server: previewServer},
	)
	cleanupContext, cancelCleanup := context.WithTimeout(context.Background(), 20*time.Second)
	server.Close(cleanupContext)
	cancelCleanup()
	if serveErr != nil {
		logger.Error("server stopped", "error", serveErr)
		os.Exit(1)
	}
}

func controlApplicationHandler(next http.Handler, config publicRuntimeConfig) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isPreviewProxyPath(r.URL.Path) {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/api/runtime-config" {
			if r.Method != http.MethodGet {
				w.Header().Set("Allow", http.MethodGet)
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(config)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func previewApplicationHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isPreviewProxyPath(r.URL.Path) {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isPreviewProxyPath(path string) bool {
	if !strings.HasPrefix(path, "/w/") {
		return false
	}
	parts := strings.SplitN(strings.TrimPrefix(path, "/w/"), "/", 3)
	return len(parts) >= 2 && parts[0] != "" && parts[1] == "port"
}

func normalizePreviewOrigin(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse VIBEDOCK_PREVIEW_ORIGIN: %w", err)
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User != nil {
		return "", errors.New("VIBEDOCK_PREVIEW_ORIGIN must be an http(s) origin without credentials")
	}
	if (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", errors.New("VIBEDOCK_PREVIEW_ORIGIN may not contain a path, query, or fragment")
	}
	return parsed.Scheme + "://" + parsed.Host, nil
}

func parsePortEnv(key string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	port, err := strconv.Atoi(raw)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("%s must be an integer between 1 and 65535", key)
	}
	return port, nil
}

func serveHTTPServers(ctx context.Context, shutdownTimeout time.Duration, endpoints ...httpEndpoint) error {
	listeners := make([]net.Listener, 0, len(endpoints))
	for _, endpoint := range endpoints {
		listener, err := net.Listen("tcp", endpoint.server.Addr)
		if err != nil {
			for _, opened := range listeners {
				_ = opened.Close()
			}
			return fmt.Errorf("listen on %s endpoint %s: %w", endpoint.name, endpoint.server.Addr, err)
		}
		listeners = append(listeners, listener)
	}

	type serveResult struct {
		name string
		err  error
	}
	results := make(chan serveResult, len(endpoints))
	for index, endpoint := range endpoints {
		listener := listeners[index]
		go func(endpoint httpEndpoint, listener net.Listener) {
			results <- serveResult{name: endpoint.name, err: endpoint.server.Serve(listener)}
		}(endpoint, listener)
	}

	var serveErr error
	select {
	case <-ctx.Done():
	case result := <-results:
		if result.err == nil || errors.Is(result.err, http.ErrServerClosed) {
			serveErr = fmt.Errorf("%s endpoint stopped unexpectedly", result.name)
		} else {
			serveErr = fmt.Errorf("serve %s endpoint: %w", result.name, result.err)
		}
	}

	shutdownContext, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	shutdownErrors := make(chan error, len(endpoints))
	var shutdownGroup sync.WaitGroup
	for _, endpoint := range endpoints {
		shutdownGroup.Add(1)
		go func(endpoint httpEndpoint) {
			defer shutdownGroup.Done()
			if err := endpoint.server.Shutdown(shutdownContext); err != nil {
				_ = endpoint.server.Close()
				shutdownErrors <- fmt.Errorf("shutdown %s endpoint: %w", endpoint.name, err)
			}
		}(endpoint)
	}
	shutdownGroup.Wait()
	close(shutdownErrors)
	for err := range shutdownErrors {
		serveErr = errors.Join(serveErr, err)
	}
	return serveErr
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

func parseBoolEnv(key string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
