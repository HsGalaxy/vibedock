package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestControlApplicationHandlerSeparatesPreviewAndServesRuntimeConfig(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})
	handler := controlApplicationHandler(next, publicRuntimeConfig{
		PreviewOrigin: "https://preview.example:8443",
		PreviewPort:   8788,
	})

	preview := httptest.NewRecorder()
	handler.ServeHTTP(preview, httptest.NewRequest(http.MethodGet, "/w/abcdef123456/port/3000/", nil))
	if preview.Code != http.StatusNotFound || called {
		t.Fatalf("control listener preview response = %d, downstream called = %v", preview.Code, called)
	}

	runtimeConfig := httptest.NewRecorder()
	handler.ServeHTTP(runtimeConfig, httptest.NewRequest(http.MethodGet, "/api/runtime-config", nil))
	if runtimeConfig.Code != http.StatusOK {
		t.Fatalf("runtime config response = %d: %s", runtimeConfig.Code, runtimeConfig.Body.String())
	}
	var payload publicRuntimeConfig
	if err := json.Unmarshal(runtimeConfig.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.PreviewOrigin != "https://preview.example:8443" || payload.PreviewPort != 8788 {
		t.Fatalf("unexpected runtime config: %#v", payload)
	}
	if cacheControl := runtimeConfig.Header().Get("Cache-Control"); cacheControl != "no-store" {
		t.Fatalf("runtime config Cache-Control = %q", cacheControl)
	}

	called = false
	code := httptest.NewRecorder()
	handler.ServeHTTP(code, httptest.NewRequest(http.MethodGet, "/w/abcdef123456/code/", nil))
	if code.Code != http.StatusNoContent || !called {
		t.Fatalf("code route response = %d, downstream called = %v", code.Code, called)
	}
}

func TestPreviewApplicationHandlerAllowsOnlyPreviewPaths(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})
	handler := previewApplicationHandler(next)

	for _, path := range []string{"/", "/api/health", "/w/abcdef123456/code/", "/w/abcdef123456/terminal/session/"} {
		called = false
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusNotFound || called {
			t.Fatalf("path %q response = %d, downstream called = %v", path, response.Code, called)
		}
	}

	called = false
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/w/abcdef123456/port/5173/", nil))
	if response.Code != http.StatusNoContent || !called {
		t.Fatalf("preview response = %d, downstream called = %v", response.Code, called)
	}
}

func TestPreviewProxyPathClassification(t *testing.T) {
	tests := map[string]bool{
		"/w/abcdef123456/port":       true,
		"/w/abcdef123456/port/3000/": true,
		"/w/abcdef123456/code/":      false,
		"/w//port/3000/":             false,
		"/w/abcdef123456/ports/3000": false,
		"/api/workspaces":            false,
	}
	for path, expected := range tests {
		if actual := isPreviewProxyPath(path); actual != expected {
			t.Errorf("isPreviewProxyPath(%q) = %v, want %v", path, actual, expected)
		}
	}
}

func TestNormalizePreviewOrigin(t *testing.T) {
	for _, test := range []struct {
		input string
		want  string
		ok    bool
	}{
		{input: "", want: "", ok: true},
		{input: " HTTPS://preview.example:8443/ ", want: "https://preview.example:8443", ok: true},
		{input: "http://127.0.0.1:8788", want: "http://127.0.0.1:8788", ok: true},
		{input: "//preview.example", ok: false},
		{input: "ftp://preview.example", ok: false},
		{input: "https://user:pass@preview.example", ok: false},
		{input: "https://preview.example/path", ok: false},
		{input: "https://preview.example?query=1", ok: false},
	} {
		actual, err := normalizePreviewOrigin(test.input)
		if test.ok && (err != nil || actual != test.want) {
			t.Errorf("normalizePreviewOrigin(%q) = %q, %v; want %q", test.input, actual, err, test.want)
		}
		if !test.ok && err == nil {
			t.Errorf("normalizePreviewOrigin(%q) unexpectedly succeeded with %q", test.input, actual)
		}
	}
}

func TestParsePortEnv(t *testing.T) {
	t.Setenv("TEST_PORT", "")
	if port, err := parsePortEnv("TEST_PORT", 8788); err != nil || port != 8788 {
		t.Fatalf("default port = %d, %v", port, err)
	}
	t.Setenv("TEST_PORT", "8443")
	if port, err := parsePortEnv("TEST_PORT", 8788); err != nil || port != 8443 {
		t.Fatalf("configured port = %d, %v", port, err)
	}
	for _, value := range []string{"0", "65536", "not-a-port"} {
		t.Setenv("TEST_PORT", value)
		if _, err := parsePortEnv("TEST_PORT", 8788); err == nil {
			t.Fatalf("invalid port %q was accepted", value)
		}
	}
}

func TestServeHTTPServersReturnsWhenOneListenerCannotStart(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()

	err = serveHTTPServers(context.Background(), time.Second,
		httpEndpoint{name: "control", server: &http.Server{Addr: "127.0.0.1:0"}},
		httpEndpoint{name: "preview", server: &http.Server{Addr: occupied.Addr().String()}},
	)
	if err == nil || !strings.Contains(err.Error(), "preview") {
		t.Fatalf("listener conflict error = %v", err)
	}
}
