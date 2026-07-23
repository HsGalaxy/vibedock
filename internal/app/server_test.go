package app

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"vibedock/internal/controlauth"
	"vibedock/internal/model"
	"vibedock/internal/secrets"
	"vibedock/internal/store"
)

func testKeyring(t *testing.T) *secrets.Keyring {
	t.Helper()
	keyring, err := secrets.New(filepath.Join(t.TempDir(), "credential.key"), "")
	if err != nil {
		t.Fatal(err)
	}
	return keyring
}

type fakeRuntime struct {
	created         bool
	cloned          string
	copied          bool
	sessions        map[string]bool
	proxyTarget     string
	captured        string
	lastSession     model.SessionRuntimeRequest
	deletedSessID   string
	stats           model.RuntimeResourceStats
	statsErr        error
	statsTarget     string
	lastCreate      model.CreateRuntimeRequest
	lastSetup       model.EnvironmentSetup
	initialized     bool
	initializeErr   error
	ensureErr       error
	ensureStarted   chan struct{}
	ensureRelease   chan struct{}
	ensureCallMu    sync.Mutex
	ensureCallCount int
}

func (f *fakeRuntime) Create(_ context.Context, req model.CreateRuntimeRequest) (model.RuntimeInfo, error) {
	f.created = true
	f.lastCreate = req
	return model.RuntimeInfo{Status: "running"}, nil
}
func (f *fakeRuntime) InitializeEnvironment(_ context.Context, _ string, setup model.EnvironmentSetup) error {
	f.initialized = true
	f.lastSetup = setup
	return f.initializeErr
}
func (f *fakeRuntime) Start(context.Context, string) (model.RuntimeInfo, error) {
	return model.RuntimeInfo{Status: "running"}, nil
}
func (f *fakeRuntime) Stop(context.Context, string) error         { return nil }
func (f *fakeRuntime) Delete(context.Context, string, bool) error { return nil }
func (f *fakeRuntime) Status(context.Context, string) (model.RuntimeInfo, error) {
	return model.RuntimeInfo{Status: "running"}, nil
}
func (f *fakeRuntime) CopyDirectory(context.Context, string, string) error {
	f.copied = true
	return nil
}
func (f *fakeRuntime) CloneGit(_ context.Context, _ string, gitURL string) error {
	f.cloned = gitURL
	return nil
}
func (f *fakeRuntime) ProxyTarget(context.Context, string, int) (string, error) {
	return f.proxyTarget, nil
}
func (f *fakeRuntime) EnsureSession(_ context.Context, _ string, req model.SessionRuntimeRequest) error {
	f.ensureCallMu.Lock()
	f.ensureCallCount++
	started, release := f.ensureStarted, f.ensureRelease
	f.ensureCallMu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if release != nil {
		<-release
	}
	if f.sessions == nil {
		f.sessions = map[string]bool{}
	}
	f.sessions[req.SessionID] = true
	f.lastSession = req
	return f.ensureErr
}
func (f *fakeRuntime) RestartSession(ctx context.Context, container string, req model.SessionRuntimeRequest) error {
	return f.EnsureSession(ctx, container, req)
}
func (f *fakeRuntime) DeleteSession(_ context.Context, _ string, sessionID string) error {
	delete(f.sessions, sessionID)
	f.deletedSessID = sessionID
	return nil
}
func (f *fakeRuntime) SessionStatus(context.Context, string, string) (string, error) {
	return "running", nil
}
func (f *fakeRuntime) CaptureSession(context.Context, string, string, int) (string, error) {
	return f.captured, nil
}
func (f *fakeRuntime) ResourceStats(_ context.Context, containerName string) (model.RuntimeResourceStats, error) {
	f.statsTarget = containerName
	return f.stats, f.statsErr
}

func TestParseEnvironment(t *testing.T) {
	values, keys, err := parseEnvironment("OPENAI_API_KEY=test\n# comment\nPORT=3000\n")
	if err != nil {
		t.Fatal(err)
	}
	if values["PORT"] != "3000" || len(keys) != 2 {
		t.Fatalf("unexpected environment: %#v %#v", values, keys)
	}
	if _, _, err := parseEnvironment("1INVALID=x"); err == nil {
		t.Fatal("expected invalid key error")
	}
}

func TestSanitizeRelativePath(t *testing.T) {
	cases := map[string]string{
		"src/main.go":    "src/main.go",
		"./README.md":    "README.md",
		"../secret":      "",
		"/absolute/path": "absolute/path",
		`dir\\file.txt`:  "dir/file.txt",
	}
	for input, expected := range cases {
		if got := sanitizeRelativePath(input); got != expected {
			t.Errorf("sanitizeRelativePath(%q) = %q, want %q", input, got, expected)
		}
	}
}

func TestRandomIDMatchesRouteConstraint(t *testing.T) {
	for range 20 {
		id, err := randomID()
		if err != nil {
			t.Fatal(err)
		}
		if !idPattern.MatchString(id) {
			t.Fatalf("generated invalid id %q", id)
		}
	}
}

func TestCreateWorkspaceCreatesInitialSession(t *testing.T) {
	temp := t.TempDir()
	st, err := store.New(filepath.Join(temp, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	runtime := &fakeRuntime{}
	web := os.DirFS(temp)
	if err := os.WriteFile(filepath.Join(temp, "index.html"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	server := NewServer(st, runtime, []model.Template{{ID: "node", Name: "Node", Image: "test/image"}}, web, slog.New(slog.NewTextHandler(os.Stderr, nil)), testKeyring(t))

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	fields := map[string]string{
		"name": "demo", "template_id": "node", "agent": "codex", "cpus": "2",
		"memory_mb": "2048", "git_url": "https://example.com/repo.git",
	}
	for key, value := range fields {
		_ = writer.WriteField(key, value)
	}
	file, err := writer.CreateFormFile("files", "README.md")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.Write([]byte("hello"))
	_ = writer.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/workspaces", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	res := httptest.NewRecorder()
	server.Handler().ServeHTTP(res, req)

	if res.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", res.Code, res.Body.String())
	}
	if !runtime.created || !runtime.copied || runtime.cloned == "" {
		t.Fatalf("runtime calls missing: %#v", runtime)
	}
	if len(runtime.sessions) != 1 || runtime.lastSession.Agent != "codex" || runtime.lastSession.Port != firstSessionPort {
		t.Fatalf("unexpected session runtime call: %#v", runtime.lastSession)
	}
	if !strings.Contains(res.Body.String(), `"sessions":[`) || !strings.Contains(res.Body.String(), `"agent":"codex"`) {
		t.Fatalf("unexpected body: %s", res.Body.String())
	}
}

func TestCreateWorkspaceAppliesInitialClaudePermissionMode(t *testing.T) {
	temp := t.TempDir()
	st, err := store.New(filepath.Join(temp, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	runtime := &fakeRuntime{}
	web := os.DirFS(temp)
	if err := os.WriteFile(filepath.Join(temp, "index.html"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	server := NewServer(st, runtime, []model.Template{{ID: "node", Name: "Node", Image: "test/image"}}, web, slog.Default(), testKeyring(t))

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, value := range map[string]string{
		"name": "claude-demo", "template_id": "node", "agent": "claude",
		"permission_mode": "acceptEdits", "cpus": "2", "memory_mb": "2048",
	} {
		if err := writer.WriteField(key, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/workspaces", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	res := httptest.NewRecorder()
	server.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", res.Code, res.Body.String())
	}
	workspaces := st.List()
	if len(workspaces) != 1 || len(workspaces[0].Sessions) != 1 {
		t.Fatalf("unexpected workspaces: %#v", workspaces)
	}
	if got := workspaces[0].Sessions[0].PermissionMode; got != "acceptEdits" {
		t.Fatalf("permission mode = %q, want acceptEdits", got)
	}
	if runtime.lastSession.PermissionMode != "acceptEdits" || runtime.lastSession.DangerousSkipPermissions {
		t.Fatalf("unexpected runtime permissions: %#v", runtime.lastSession)
	}
}

func TestCreateWorkspaceDefaultsInitialClaudePermissionMode(t *testing.T) {
	temp := t.TempDir()
	st, err := store.New(filepath.Join(temp, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	runtime := &fakeRuntime{}
	web := os.DirFS(temp)
	if err := os.WriteFile(filepath.Join(temp, "index.html"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	server := NewServer(st, runtime, []model.Template{{ID: "node", Name: "Node", Image: "test/image"}}, web, slog.Default(), testKeyring(t))

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, value := range map[string]string{"name": "claude-default", "template_id": "node", "agent": "claude"} {
		if err := writer.WriteField(key, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/workspaces", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	res := httptest.NewRecorder()
	server.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", res.Code, res.Body.String())
	}
	workspaces := st.List()
	if len(workspaces) != 1 || len(workspaces[0].Sessions) != 1 {
		t.Fatalf("unexpected workspaces: %#v", workspaces)
	}
	if got := workspaces[0].Sessions[0].PermissionMode; got != "manual" {
		t.Fatalf("permission mode = %q, want manual", got)
	}
	if runtime.lastSession.PermissionMode != "manual" || runtime.lastSession.DangerousSkipPermissions {
		t.Fatalf("unexpected runtime permissions: %#v", runtime.lastSession)
	}
}

func TestCreateWorkspaceRejectsClaudeBypassWithoutKnownWorkspaceID(t *testing.T) {
	temp := t.TempDir()
	st, _ := store.New(filepath.Join(temp, "state.json"))
	web := os.DirFS(temp)
	_ = os.WriteFile(filepath.Join(temp, "index.html"), []byte("ok"), 0o644)
	server := NewServer(st, &fakeRuntime{}, []model.Template{{ID: "node", Name: "Node", Image: "test/image"}}, web, slog.Default(), testKeyring(t))

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, value := range map[string]string{"name": "demo", "template_id": "node", "agent": "claude", "permission_mode": "bypassPermissions"} {
		_ = writer.WriteField(key, value)
	}
	_ = writer.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/workspaces", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	res := httptest.NewRecorder()
	server.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest || !strings.Contains(res.Body.String(), "权限配置无效") {
		t.Fatalf("unexpected response: %d %s", res.Code, res.Body.String())
	}
	if len(st.List()) != 0 {
		t.Fatal("invalid permission request created a Workspace")
	}
}

func TestValidateSessionPermissionsRejectsEmptyWorkspaceConfirmation(t *testing.T) {
	for _, dangerous := range []bool{false, true} {
		mode := "manual"
		if !dangerous {
			mode = "bypassPermissions"
		}
		if _, err := validateSessionPermissions("claude", mode, dangerous, "", ""); err == nil {
			t.Fatalf("expected empty Workspace ID to reject dangerous=%t mode=%q", dangerous, mode)
		}
	}
	if got, err := validateSessionPermissions("claude", "manual", false, "", ""); err != nil || got != "manual" {
		t.Fatalf("safe default rejected: mode=%q err=%v", got, err)
	}
	if got, err := validateSessionPermissions("claude", "manual", true, "abcdef123456", "abcdef123456"); err != nil || got != "bypassPermissions" {
		t.Fatalf("confirmed dangerous mode rejected: mode=%q err=%v", got, err)
	}
}

func TestCreateAndDeleteSession(t *testing.T) {
	temp := t.TempDir()
	st, _ := store.New(filepath.Join(temp, "state.json"))
	runtime := &fakeRuntime{}
	web := os.DirFS(temp)
	_ = os.WriteFile(filepath.Join(temp, "index.html"), []byte("ok"), 0o644)
	server := NewServer(st, runtime, nil, web, slog.Default(), testKeyring(t))
	ws := model.Workspace{ID: "abcdef123456", Name: "demo", ContainerName: "demo", Status: "running", Password: "pw"}
	_ = st.Put(ws)

	req := httptest.NewRequest(http.MethodPost, "/api/workspaces/abcdef123456/sessions", strings.NewReader(`{"name":"test agent","agent":"claude"}`))
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	server.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusCreated {
		t.Fatalf("create status %d: %s", res.Code, res.Body.String())
	}
	stored, _ := st.Get(ws.ID)
	if len(stored.Sessions) != 2 { // legacy migration creates one default session first
		t.Fatalf("expected two sessions after migration and create, got %#v", stored.Sessions)
	}
	created := stored.Sessions[1]

	deleteReq := httptest.NewRequest(http.MethodDelete, "/api/workspaces/abcdef123456/sessions/"+created.ID, nil)
	deleteRes := httptest.NewRecorder()
	server.Handler().ServeHTTP(deleteRes, deleteReq)
	if deleteRes.Code != http.StatusNoContent || runtime.deletedSessID != created.ID {
		t.Fatalf("delete status %d session %q", deleteRes.Code, runtime.deletedSessID)
	}
}

func TestEnsureSessionSingleflightCoalescesConcurrentCalls(t *testing.T) {
	temp := t.TempDir()
	st, _ := store.New(filepath.Join(temp, "state.json"))
	web := os.DirFS(temp)
	_ = os.WriteFile(filepath.Join(temp, "index.html"), []byte("ok"), 0o644)
	runtime := &fakeRuntime{ensureStarted: make(chan struct{}, 1), ensureRelease: make(chan struct{})}
	server := NewServer(st, runtime, nil, web, slog.Default(), testKeyring(t))
	ws := model.Workspace{ID: "abcdef123456", ContainerName: "demo", Status: "running", Password: "pw"}
	session := model.Session{ID: "123456abcdef", Agent: "shell", Port: 7700}

	const count = 8
	errors := make(chan error, count)
	for range count {
		go func() { errors <- server.ensureSessionSingleflight(context.Background(), ws, session) }()
	}
	select {
	case <-runtime.ensureStarted:
	case <-time.After(time.Second):
		t.Fatal("ensure did not start")
	}
	time.Sleep(30 * time.Millisecond)
	close(runtime.ensureRelease)
	for range count {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
	}
	runtime.ensureCallMu.Lock()
	calls := runtime.ensureCallCount
	runtime.ensureCallMu.Unlock()
	if calls != 1 {
		t.Fatalf("EnsureSession called %d times, want 1", calls)
	}
}

func TestWorkspaceProxyStripsCodePrefix(t *testing.T) {
	receivedCookies := make(chan []*http.Cookie, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedCookies <- r.Cookies()
		if r.URL.Path != "/hello" {
			t.Fatalf("upstream path %q", r.URL.Path)
		}
		w.Header().Add("Set-Cookie", controlauth.SessionCookieName+"=stolen; Path=/; HttpOnly")
		w.Header().Add("Set-Cookie", "session=x; Path=/; HttpOnly")
		_, _ = w.Write([]byte("proxied"))
	}))
	defer upstream.Close()

	temp := t.TempDir()
	st, _ := store.New(filepath.Join(temp, "state.json"))
	runtime := &fakeRuntime{proxyTarget: upstream.URL}
	web := os.DirFS(temp)
	_ = os.WriteFile(filepath.Join(temp, "index.html"), []byte("ok"), 0o644)
	server := NewServer(st, runtime, nil, web, slog.Default(), testKeyring(t))
	_ = st.Put(model.Workspace{ID: "abcdef123456", ContainerName: "demo", Status: "running"})

	req := httptest.NewRequest(http.MethodGet, "/w/abcdef123456/code/hello", nil)
	req.AddCookie(&http.Cookie{Name: controlauth.SessionCookieName, Value: "control-secret"})
	req.AddCookie(&http.Cookie{Name: "session", Value: "tool-value"})
	res := httptest.NewRecorder()
	server.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusOK || res.Body.String() != "proxied" {
		t.Fatalf("proxy status %d body %q", res.Code, res.Body.String())
	}
	upstreamCookies := <-receivedCookies
	if len(upstreamCookies) != 1 || upstreamCookies[0].Name != "session" || upstreamCookies[0].Value != "tool-value" {
		t.Fatalf("code upstream received sensitive or unexpected cookies: %#v", upstreamCookies)
	}
	responseCookies := res.Header().Values("Set-Cookie")
	if len(responseCookies) != 1 || strings.Contains(responseCookies[0], controlauth.SessionCookieName) || !strings.Contains(responseCookies[0], "Path=/w/abcdef123456/code/") {
		t.Fatalf("code response cookies were not safely filtered and scoped: %q", responseCookies)
	}
}

func TestWorkspaceDoesNotExposeInternalPassword(t *testing.T) {
	temp := t.TempDir()
	st, _ := store.New(filepath.Join(temp, "state.json"))
	web := os.DirFS(temp)
	_ = os.WriteFile(filepath.Join(temp, "index.html"), []byte("ok"), 0o644)
	server := NewServer(st, &fakeRuntime{}, nil, web, slog.Default(), testKeyring(t))
	_ = st.Put(model.Workspace{ID: "abcdef123456", Name: "demo", ContainerName: "demo", Password: "internal"})
	res := httptest.NewRecorder()
	server.Handler().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/workspaces/abcdef123456", nil))
	if strings.Contains(res.Body.String(), "internal") {
		t.Fatalf("internal workspace credential leaked: %s", res.Body.String())
	}
}

func TestWorkspaceStatsReturnsPublicRuntimeData(t *testing.T) {
	temp := t.TempDir()
	st, _ := store.New(filepath.Join(temp, "state.json"))
	web := os.DirFS(temp)
	_ = os.WriteFile(filepath.Join(temp, "index.html"), []byte("ok"), 0o644)
	cpu, cpuLimit := 12.5, 2.0
	memory, memoryLimit, memoryPercent, pids := uint64(512<<20), uint64(4<<30), 12.5, uint64(24)
	runtime := &fakeRuntime{stats: model.RuntimeResourceStats{
		ContainerState: "running", Live: true, CPUPercent: &cpu, CPULimit: &cpuLimit,
		MemoryBytes: &memory, MemoryLimit: &memoryLimit, MemoryPercent: &memoryPercent, PIDs: &pids,
		SampledAt: time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC),
	}}
	server := NewServer(st, runtime, nil, web, slog.Default(), testKeyring(t))
	_ = st.Put(model.Workspace{ID: "abcdef123456", ContainerName: "private-container", CPUs: 2, MemoryMB: 4096, Password: "internal-secret"})

	res := httptest.NewRecorder()
	server.Handler().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/workspaces/abcdef123456/stats", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("stats status %d: %s", res.Code, res.Body.String())
	}
	body := res.Body.String()
	for _, expected := range []string{`"workspace_id":"abcdef123456"`, `"container_state":"running"`, `"cpu_percent":12.5`, `"memory_bytes":536870912`, `"cpu_cores":2`} {
		if !strings.Contains(body, expected) {
			t.Fatalf("stats body missing %s: %s", expected, body)
		}
	}
	for _, secret := range []string{"private-container", "internal-secret", "password"} {
		if strings.Contains(body, secret) {
			t.Fatalf("stats leaked %q: %s", secret, body)
		}
	}
	if runtime.statsTarget != "private-container" || res.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("unexpected target/header: %q %q", runtime.statsTarget, res.Header().Get("Cache-Control"))
	}
}

func TestWorkspaceStatsReportsUnavailableContainer(t *testing.T) {
	temp := t.TempDir()
	st, _ := store.New(filepath.Join(temp, "state.json"))
	web := os.DirFS(temp)
	_ = os.WriteFile(filepath.Join(temp, "index.html"), []byte("ok"), 0o644)
	server := NewServer(st, &fakeRuntime{stats: model.RuntimeResourceStats{ContainerState: "missing", Unavailable: "container_missing", SampledAt: time.Now().UTC()}}, nil, web, slog.Default(), testKeyring(t))
	_ = st.Put(model.Workspace{ID: "abcdef123456", ContainerName: "missing"})
	res := httptest.NewRecorder()
	server.Handler().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/workspaces/abcdef123456/stats", nil))
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"usage":null`) || !strings.Contains(res.Body.String(), `"unavailable_reason":"container_missing"`) {
		t.Fatalf("unexpected unavailable stats: %d %s", res.Code, res.Body.String())
	}
}

func TestValidateGitURLRejectsCredentialsAndHTTP(t *testing.T) {
	for _, raw := range []string{"https://user:secret@example.com/repo.git", "http://example.com/repo.git"} {
		if err := validateGitURL(raw); err == nil {
			t.Fatalf("validateGitURL accepted unsafe URL %q", raw)
		}
	}
	if err := validateGitURL("https://github.com/example/repo.git"); err != nil {
		t.Fatalf("validateGitURL rejected safe URL: %v", err)
	}
}

var _ fs.FS = os.DirFS(".")

func TestManagedAccountIsEncryptedAndInjectedIntoSession(t *testing.T) {
	temp := t.TempDir()
	st, err := store.New(filepath.Join(temp, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	runtime := &fakeRuntime{}
	web := os.DirFS(temp)
	_ = os.WriteFile(filepath.Join(temp, "index.html"), []byte("ok"), 0o644)
	keyring := testKeyring(t)
	server := NewServer(st, runtime, nil, web, slog.Default(), keyring)

	accountReq := httptest.NewRequest(http.MethodPost, "/api/accounts", strings.NewReader(`{
		"name":"OpenAI relay",
		"agent":"codex",
		"auth_type":"api_key",
		"base_url":"https://relay.example/v1/",
		"model":"gpt-test",
		"secret":"sk-super-secret",
		"extra_env":{"PROVIDER_REGION":"jp"}
	}`))
	accountReq.Header.Set("Content-Type", "application/json")
	accountRes := httptest.NewRecorder()
	server.Handler().ServeHTTP(accountRes, accountReq)
	if accountRes.Code != http.StatusCreated {
		t.Fatalf("account status %d: %s", accountRes.Code, accountRes.Body.String())
	}
	if strings.Contains(accountRes.Body.String(), "sk-super-secret") {
		t.Fatalf("secret leaked in response: %s", accountRes.Body.String())
	}
	accounts := st.ListAccounts()
	if len(accounts) != 1 || accounts[0].SecretCiphertext == "" || strings.Contains(accounts[0].SecretCiphertext, "sk-super-secret") {
		t.Fatalf("secret was not encrypted at rest: %#v", accounts)
	}

	ws := model.Workspace{ID: "abcdef123456", Name: "demo", ContainerName: "demo", Status: "running", Password: "pw"}
	_ = st.Put(ws)
	body := `{"name":"managed","agent":"codex","account_id":"` + accounts[0].ID + `"}`
	sessionReq := httptest.NewRequest(http.MethodPost, "/api/workspaces/abcdef123456/sessions", strings.NewReader(body))
	sessionReq.Header.Set("Content-Type", "application/json")
	sessionRes := httptest.NewRecorder()
	server.Handler().ServeHTTP(sessionRes, sessionReq)
	if sessionRes.Code != http.StatusCreated {
		t.Fatalf("session status %d: %s", sessionRes.Code, sessionRes.Body.String())
	}
	if runtime.lastSession.Account.ID != accounts[0].ID || runtime.lastSession.Account.Secret != "sk-super-secret" {
		t.Fatalf("account was not resolved for runtime: %#v", runtime.lastSession.Account)
	}
	if runtime.lastSession.Account.BaseURL != "https://relay.example/v1" || runtime.lastSession.Account.Model != "gpt-test" {
		t.Fatalf("provider settings missing: %#v", runtime.lastSession.Account)
	}
}

func TestAccountCannotBeDeletedWhileReferenced(t *testing.T) {
	temp := t.TempDir()
	st, _ := store.New(filepath.Join(temp, "state.json"))
	web := os.DirFS(temp)
	_ = os.WriteFile(filepath.Join(temp, "index.html"), []byte("ok"), 0o644)
	server := NewServer(st, &fakeRuntime{}, nil, web, slog.Default(), testKeyring(t))
	now := time.Now().UTC()
	account := model.AccountProfile{ID: "acct12345678", Name: "Claude", Agent: "claude", AuthType: "login", CreatedAt: now, UpdatedAt: now}
	if err := st.PutAccount(account); err != nil {
		t.Fatal(err)
	}
	if err := st.Put(model.Workspace{ID: "abcdef123456", Sessions: []model.Session{{ID: "session123456", Agent: "claude", AccountID: account.ID}}}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodDelete, "/api/accounts/"+account.ID, nil)
	res := httptest.NewRecorder()
	server.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusConflict {
		t.Fatalf("delete status %d: %s", res.Code, res.Body.String())
	}
}

func TestAccountRejectsSecretLikeExtraEnvironment(t *testing.T) {
	temp := t.TempDir()
	st, _ := store.New(filepath.Join(temp, "state.json"))
	web := os.DirFS(temp)
	_ = os.WriteFile(filepath.Join(temp, "index.html"), []byte("ok"), 0o644)
	server := NewServer(st, &fakeRuntime{}, nil, web, slog.Default(), testKeyring(t))

	req := httptest.NewRequest(http.MethodPost, "/api/accounts", strings.NewReader(`{
		"name":"unsafe relay",
		"agent":"codex",
		"auth_type":"api_key",
		"secret":"primary-secret",
		"extra_env":{"SECONDARY_API_KEY":"must-not-be-plaintext"}
	}`))
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	server.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("account status %d: %s", res.Code, res.Body.String())
	}
	if len(st.ListAccounts()) != 0 {
		t.Fatal("invalid account was persisted")
	}
}

func TestPiSessionUsesEncryptedDirectParametersWithoutAccount(t *testing.T) {
	temp := t.TempDir()
	st, err := store.New(filepath.Join(temp, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	runtime := &fakeRuntime{}
	web := os.DirFS(temp)
	_ = os.WriteFile(filepath.Join(temp, "index.html"), []byte("ok"), 0o644)
	server := NewServer(st, runtime, nil, web, slog.Default(), testKeyring(t))
	ws := model.Workspace{ID: "abcdef123456", Name: "demo", ContainerName: "demo", Status: "running", Password: "pw"}
	if err := st.Put(ws); err != nil {
		t.Fatal(err)
	}

	body := `{
		"name":"pi relay",
		"agent":"pi",
		"account_id":"",
		"direct_config":{
			"provider":"relay",
			"base_url":"https://relay.example/v1/",
			"model":"gpt-test",
			"protocol":"openai-responses",
			"thinking":"high",
			"auto_approve":true,
			"secret":"sk-direct-secret"
		}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/workspaces/abcdef123456/sessions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	server.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusCreated {
		t.Fatalf("session status %d: %s", res.Code, res.Body.String())
	}
	if strings.Contains(res.Body.String(), "sk-direct-secret") {
		t.Fatalf("direct secret leaked in response: %s", res.Body.String())
	}
	if runtime.lastSession.Agent != "pi" || runtime.lastSession.Account.ID != "" {
		t.Fatalf("unexpected runtime identity: %#v", runtime.lastSession)
	}
	direct := runtime.lastSession.Direct
	if direct.Secret != "sk-direct-secret" || direct.Provider != "relay" || direct.BaseURL != "https://relay.example/v1" || direct.Model != "gpt-test" || direct.Protocol != "openai-responses" || direct.Thinking != "high" || !direct.AutoApprove {
		t.Fatalf("direct parameters missing: %#v", direct)
	}
	stored, _ := st.Get(ws.ID)
	created := stored.Sessions[len(stored.Sessions)-1]
	if created.AccountID != "" || created.DirectConfig == nil || !created.DirectConfig.HasSecret || created.DirectConfig.SecretHint != "••••cret" {
		t.Fatalf("unexpected stored direct config: %#v", created)
	}
	ciphertext, ok := st.GetSessionSecret(created.ID)
	if !ok || ciphertext == "" || strings.Contains(ciphertext, "sk-direct-secret") {
		t.Fatalf("direct secret was not encrypted: %q", ciphertext)
	}
}

func TestDirectAgentRejectsIncompleteProviderConfiguration(t *testing.T) {
	temp := t.TempDir()
	st, _ := store.New(filepath.Join(temp, "state.json"))
	web := os.DirFS(temp)
	_ = os.WriteFile(filepath.Join(temp, "index.html"), []byte("ok"), 0o644)
	server := NewServer(st, &fakeRuntime{}, nil, web, slog.Default(), testKeyring(t))
	_ = st.Put(model.Workspace{ID: "abcdef123456", ContainerName: "demo", Status: "running", Password: "pw"})

	for _, body := range []string{
		`{"agent":"pi","direct_config":{}}`,
		`{"agent":"pi","direct_config":{"provider":"relay","model":"gpt-test","secret":"sk-test"}}`,
		`{"agent":"opencode","direct_config":{"provider":"relay","base_url":"https://relay.example/v1","model":"gpt-test"}}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/workspaces/abcdef123456/sessions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		res := httptest.NewRecorder()
		server.Handler().ServeHTTP(res, req)
		if res.Code != http.StatusBadRequest {
			t.Fatalf("expected incomplete direct config rejection, got %d: %s", res.Code, res.Body.String())
		}
	}
}

func TestCreateSessionReturnsPersistedRuntimeError(t *testing.T) {
	temp := t.TempDir()
	st, _ := store.New(filepath.Join(temp, "state.json"))
	web := os.DirFS(temp)
	_ = os.WriteFile(filepath.Join(temp, "index.html"), []byte("ok"), 0o644)
	runtime := &fakeRuntime{ensureErr: errors.New("agent executable missing")}
	server := NewServer(st, runtime, nil, web, slog.Default(), testKeyring(t))
	_ = st.Put(model.Workspace{ID: "abcdef123456", ContainerName: "demo", Status: "running", Password: "pw"})

	req := httptest.NewRequest(http.MethodPost, "/api/workspaces/abcdef123456/sessions", strings.NewReader(`{"name":"broken","agent":"claude"}`))
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	server.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusCreated || !strings.Contains(res.Body.String(), `"status":"error"`) || !strings.Contains(res.Body.String(), "agent executable missing") {
		t.Fatalf("expected created error session, got %d: %s", res.Code, res.Body.String())
	}
	stored, _ := st.Get("abcdef123456")
	created := stored.Sessions[len(stored.Sessions)-1]
	if created.Status != "error" || created.Error == "" {
		t.Fatalf("runtime error was not persisted: %#v", created)
	}
}

func TestLegacyIncompleteDirectSessionFailsReadiness(t *testing.T) {
	temp := t.TempDir()
	st, _ := store.New(filepath.Join(temp, "state.json"))
	web := os.DirFS(temp)
	_ = os.WriteFile(filepath.Join(temp, "index.html"), []byte("ok"), 0o644)
	server := NewServer(st, &fakeRuntime{}, nil, web, slog.Default(), testKeyring(t))
	session := model.Session{ID: "session123456", Agent: "opencode", DirectConfig: &model.DirectAgentConfig{Provider: "vibedock", Protocol: "openai-completions"}}
	if _, err := server.directRuntime(session); err == nil || !strings.Contains(err.Error(), "配置不完整") {
		t.Fatalf("expected legacy direct session readiness error, got %v", err)
	}
	ws := model.Workspace{ID: "abcdef123456", ContainerName: "demo", Status: "running", Sessions: []model.Session{session}}
	refreshed := server.refreshWorkspace(context.Background(), ws, false)
	if refreshed.Sessions[0].Status != "error" || !strings.Contains(refreshed.Sessions[0].Error, "配置不完整") {
		t.Fatalf("refresh hid legacy readiness error: %#v", refreshed.Sessions[0])
	}
}

func TestDirectAgentRejectsManagedAccount(t *testing.T) {
	temp := t.TempDir()
	st, _ := store.New(filepath.Join(temp, "state.json"))
	web := os.DirFS(temp)
	_ = os.WriteFile(filepath.Join(temp, "index.html"), []byte("ok"), 0o644)
	server := NewServer(st, &fakeRuntime{}, nil, web, slog.Default(), testKeyring(t))
	now := time.Now().UTC()
	account := model.AccountProfile{ID: "acct12345678", Name: "OpenAI", Agent: "codex", AuthType: "login", CreatedAt: now, UpdatedAt: now}
	_ = st.PutAccount(account)
	_ = st.Put(model.Workspace{ID: "abcdef123456", ContainerName: "demo", Status: "running", Password: "pw"})
	req := httptest.NewRequest(http.MethodPost, "/api/workspaces/abcdef123456/sessions", strings.NewReader(`{"agent":"opencode","account_id":"acct12345678","direct_config":{"provider":"openai","model":"gpt-test"}}`))
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	server.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("expected managed account rejection, got %d: %s", res.Code, res.Body.String())
	}
}

func TestAllowWorkbenchEmbedding(t *testing.T) {
	header := http.Header{}
	header.Set("X-Frame-Options", "DENY")
	header.Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; script-src 'self'")
	allowWorkbenchEmbedding(header)
	if got := header.Get("X-Frame-Options"); got != "" {
		t.Fatalf("X-Frame-Options was not removed: %q", got)
	}
	csp := header.Get("Content-Security-Policy")
	if strings.Contains(strings.ToLower(csp), "frame-ancestors") {
		t.Fatalf("frame-ancestors was not removed: %q", csp)
	}
	if !strings.Contains(csp, "default-src 'self'") || !strings.Contains(csp, "script-src 'self'") {
		t.Fatalf("unrelated CSP directives were lost: %q", csp)
	}
}

func TestWorkspacePreviewProxyAllowsEmbeddingAndScopesCookies(t *testing.T) {
	receivedCookies := make(chan []*http.Cookie, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app" {
			t.Fatalf("upstream path %q", r.URL.Path)
		}
		receivedCookies <- r.Cookies()
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; img-src data:")
		w.Header().Add("Set-Cookie", controlauth.SessionCookieName+"=stolen; Path=/; HttpOnly")
		w.Header().Add("Set-Cookie", "preview=x; Path=/; HttpOnly")
		w.Header().Set("Location", "/next")
		w.WriteHeader(http.StatusFound)
	}))
	defer upstream.Close()

	temp := t.TempDir()
	st, _ := store.New(filepath.Join(temp, "state.json"))
	runtime := &fakeRuntime{proxyTarget: upstream.URL}
	web := os.DirFS(temp)
	_ = os.WriteFile(filepath.Join(temp, "index.html"), []byte("ok"), 0o644)
	server := NewServer(st, runtime, nil, web, slog.Default(), testKeyring(t))
	_ = st.Put(model.Workspace{ID: "abcdef123456", ContainerName: "demo", Status: "running"})

	req := httptest.NewRequest(http.MethodGet, "/w/abcdef123456/port/3000/app", nil)
	req.AddCookie(&http.Cookie{Name: controlauth.SessionCookieName, Value: "control-secret"})
	req.AddCookie(&http.Cookie{Name: "preview", Value: "project-value"})
	res := httptest.NewRecorder()
	server.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusFound {
		t.Fatalf("proxy status %d body %q", res.Code, res.Body.String())
	}
	if got := res.Header().Get("X-Frame-Options"); got != "" {
		t.Fatalf("X-Frame-Options not removed: %q", got)
	}
	if csp := strings.ToLower(res.Header().Get("Content-Security-Policy")); strings.Contains(csp, "frame-ancestors") || !strings.Contains(csp, "default-src") {
		t.Fatalf("unexpected CSP after rewrite: %q", csp)
	}
	upstreamCookies := <-receivedCookies
	if len(upstreamCookies) != 1 || upstreamCookies[0].Name != "preview" || upstreamCookies[0].Value != "project-value" {
		t.Fatalf("preview upstream received sensitive or unexpected cookies: %#v", upstreamCookies)
	}
	responseCookies := res.Header().Values("Set-Cookie")
	if len(responseCookies) != 1 || strings.Contains(responseCookies[0], controlauth.SessionCookieName) || !strings.Contains(responseCookies[0], "Path=/w/abcdef123456/port/3000/") {
		t.Fatalf("preview response cookies were not safely filtered and scoped: %q", responseCookies)
	}
	if location := res.Header().Get("Location"); location != "/w/abcdef123456/port/3000/next" {
		t.Fatalf("preview location not rewritten: %q", location)
	}
}
