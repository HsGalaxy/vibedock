package controlauth

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const testPassword = "correct-horse-battery"

func TestFirstRunRegistrationPersistsBcryptAndStartsSession(t *testing.T) {
	credentialPath := filepath.Join(t.TempDir(), "auth", "admin.json")
	manager := newTestManager(t, credentialPath, Options{})
	handler := manager.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("application handler should not be called")
	}))

	status := performRequest(handler, http.MethodGet, "http://example.com/api/auth/status", "", nil, nil)
	if status.Code != http.StatusOK || statusJSON(t, status).Initialized {
		t.Fatalf("unexpected initial status %d: %s", status.Code, status.Body.String())
	}
	if bytes.Contains(status.Body.Bytes(), manager.bootstrapToken) {
		t.Fatal("bootstrap token leaked through auth status")
	}

	bootstrapPath := filepath.Join(filepath.Dir(credentialPath), "bootstrap.token")
	bootstrapData, err := os.ReadFile(bootstrapPath)
	if err != nil {
		t.Fatal(err)
	}
	decodedBootstrap, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(string(bootstrapData)))
	if err != nil || len(decodedBootstrap) != 32 {
		t.Fatalf("invalid bootstrap token: %q", bootstrapData)
	}
	if info, err := os.Stat(bootstrapPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("bootstrap token permissions: info=%v err=%v", info, err)
	}
	reopenedBeforeSetup := newTestManager(t, credentialPath, Options{})
	if !bytes.Equal(reopenedBeforeSetup.bootstrapToken, manager.bootstrapToken) {
		t.Fatal("uninitialized restart replaced the bootstrap token")
	}

	registered := register(t, manager, handler, "管理员", testPassword)
	if registered.response.Code != http.StatusCreated {
		t.Fatalf("registration status %d: %s", registered.response.Code, registered.response.Body.String())
	}
	if !registered.status.Initialized || !registered.status.Authenticated || registered.status.Username != "管理员" || registered.status.CSRFToken == "" {
		t.Fatalf("unexpected registration response: %#v", registered.status)
	}
	if !registered.cookie.HttpOnly || registered.cookie.SameSite != http.SameSiteStrictMode || registered.cookie.Path != "/" || registered.cookie.MaxAge <= 0 {
		t.Fatalf("insecure session cookie: %#v", registered.cookie)
	}
	if _, err := os.Stat(bootstrapPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("bootstrap token still exists after registration: %v", err)
	}

	data, err := os.ReadFile(credentialPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(testPassword)) {
		t.Fatal("credential file contains the plaintext password")
	}
	info, err := os.Stat(credentialPath)
	if err != nil {
		t.Fatal(err)
	}
	if permissions := info.Mode().Perm(); permissions != 0o600 {
		t.Fatalf("credential mode %o, want 600", permissions)
	}
	var stored diskCredential
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Username != "管理员" || bcrypt.CompareHashAndPassword([]byte(stored.PasswordHash), []byte(testPassword)) != nil {
		t.Fatalf("invalid stored credential: %#v", stored)
	}

	withCookie := performRequest(handler, http.MethodGet, "http://example.com/api/auth/status", "", registered.cookie, nil)
	authenticatedStatus := statusJSON(t, withCookie)
	if !authenticatedStatus.Authenticated || authenticatedStatus.CSRFToken != registered.status.CSRFToken {
		t.Fatalf("unexpected authenticated status: %#v", authenticatedStatus)
	}

	reloaded := newTestManager(t, credentialPath, Options{})
	if !reloaded.Initialized() {
		t.Fatal("reloaded manager did not load the administrator")
	}
	reloadedStatus := performRequest(reloaded.Handler(http.NotFoundHandler()), http.MethodGet, "http://example.com/api/auth/status", "", registered.cookie, nil)
	if value := statusJSON(t, reloadedStatus); !value.Initialized || value.Authenticated {
		t.Fatalf("sessions should not survive restart: %#v", value)
	}
}

func TestHandlerProtectsAPIsAndWorkspaceProxy(t *testing.T) {
	manager := newTestManager(t, filepath.Join(t.TempDir(), "admin.json"), Options{})
	called := 0
	app := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		if r.URL.Path != "/" {
			if username, ok := UsernameFromContext(r.Context()); !ok || username != "admin" {
				t.Errorf("missing authenticated identity: %q, %v", username, ok)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	})
	handler := manager.Handler(app)
	registered := register(t, manager, handler, "admin", testPassword)

	for _, path := range []string{"/api/health", "/api/workspaces", "/w/workspace/code/"} {
		response := performRequest(handler, http.MethodGet, "http://example.com"+path, "", nil, nil)
		if response.Code != http.StatusUnauthorized {
			t.Errorf("%s returned %d, want 401", path, response.Code)
		}
	}
	if called != 0 {
		t.Fatalf("protected application was called %d times", called)
	}

	root := performRequest(handler, http.MethodGet, "http://example.com/", "", nil, nil)
	if root.Code != http.StatusNoContent {
		t.Fatalf("static root returned %d", root.Code)
	}
	if called != 1 {
		t.Fatalf("static application call count %d", called)
	}

	authenticated := performRequest(handler, http.MethodGet, "http://example.com/api/workspaces", "", registered.cookie, nil)
	if authenticated.Code != http.StatusNoContent {
		t.Fatalf("authenticated API returned %d: %s", authenticated.Code, authenticated.Body.String())
	}

	missingOrigin := performRequest(handler, http.MethodPost, "http://example.com/api/workspaces", "{}", registered.cookie, map[string]string{
		"X-VibeDock-CSRF": registered.status.CSRFToken,
	})
	if missingOrigin.Code != http.StatusForbidden || !strings.Contains(missingOrigin.Body.String(), "origin_not_allowed") {
		t.Fatalf("missing origin returned %d: %s", missingOrigin.Code, missingOrigin.Body.String())
	}

	missingCSRF := performRequest(handler, http.MethodPost, "http://example.com/api/workspaces", "{}", registered.cookie, map[string]string{
		"Origin": "http://example.com",
	})
	if missingCSRF.Code != http.StatusForbidden || !strings.Contains(missingCSRF.Body.String(), "csrf_invalid") {
		t.Fatalf("missing CSRF returned %d: %s", missingCSRF.Code, missingCSRF.Body.String())
	}

	crossSite := performRequest(handler, http.MethodPost, "http://example.com/api/workspaces", "{}", registered.cookie, map[string]string{
		"Origin": "http://example.com", "Sec-Fetch-Site": "cross-site", "X-VibeDock-CSRF": registered.status.CSRFToken,
	})
	if crossSite.Code != http.StatusForbidden {
		t.Fatalf("cross-site mutation returned %d", crossSite.Code)
	}

	valid := performRequest(handler, http.MethodPost, "http://example.com/api/workspaces", "{}", registered.cookie, map[string]string{
		"Origin": "http://example.com", "Sec-Fetch-Site": "same-origin", "X-VibeDock-CSRF": registered.status.CSRFToken,
	})
	if valid.Code != http.StatusNoContent {
		t.Fatalf("valid mutation returned %d: %s", valid.Code, valid.Body.String())
	}

	health := performRequest(handler, http.MethodGet, "http://example.com/healthz", "", nil, nil)
	if health.Code != http.StatusOK || strings.TrimSpace(health.Body.String()) != `{"ok":true}` {
		t.Fatalf("public health response %d: %q", health.Code, health.Body.String())
	}
}

func TestLoginLogoutAndRateLimit(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	manager := newTestManager(t, filepath.Join(t.TempDir(), "admin.json"), Options{
		MaxFailures: 2, FailureWindow: time.Minute, Now: func() time.Time { return now },
	})
	handler := manager.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	registered := register(t, manager, handler, "admin", testPassword)

	logout := performRequest(handler, http.MethodPost, "http://example.com/api/auth/logout", "", registered.cookie, map[string]string{
		"Origin": "http://example.com", "X-VibeDock-CSRF": registered.status.CSRFToken,
	})
	if logout.Code != http.StatusOK {
		t.Fatalf("logout returned %d: %s", logout.Code, logout.Body.String())
	}
	if cookies := logout.Result().Cookies(); len(cookies) != 1 || cookies[0].MaxAge >= 0 {
		t.Fatalf("logout did not clear cookie: %#v", cookies)
	}
	oldSession := performRequest(handler, http.MethodGet, "http://example.com/api/workspaces", "", registered.cookie, nil)
	if oldSession.Code != http.StatusUnauthorized {
		t.Fatalf("logged-out session returned %d", oldSession.Code)
	}

	for attempt := 0; attempt < 2; attempt++ {
		failed := login(t, handler, "admin", "wrong-password")
		if failed.Code != http.StatusUnauthorized {
			t.Fatalf("failed login %d returned %d: %s", attempt, failed.Code, failed.Body.String())
		}
	}
	limited := login(t, handler, "admin", testPassword)
	if limited.Code != http.StatusTooManyRequests || limited.Header().Get("Retry-After") == "" {
		t.Fatalf("limited login returned %d headers=%v: %s", limited.Code, limited.Header(), limited.Body.String())
	}

	now = now.Add(time.Minute + time.Second)
	success := login(t, handler, "admin", testPassword)
	if success.Code != http.StatusOK {
		t.Fatalf("successful login returned %d: %s", success.Code, success.Body.String())
	}
	value := statusJSON(t, success)
	if !value.Authenticated || value.CSRFToken == "" {
		t.Fatalf("unexpected login response: %#v", value)
	}
}

func TestInvalidShadowCookieCannotHideOrPreserveValidSession(t *testing.T) {
	manager := newTestManager(t, filepath.Join(t.TempDir(), "admin.json"), Options{})
	handler := manager.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	registered := register(t, manager, handler, "admin", testPassword)

	shadowedCookie := fmt.Sprintf("%s=shadow-value; %s=%s", SessionCookieName, SessionCookieName, registered.cookie.Value)
	request := httptest.NewRequest(http.MethodGet, "http://example.com/w/demo/code/", nil)
	request.Header.Set("Cookie", shadowedCookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("valid session hidden by shadow cookie: %d %s", response.Code, response.Body.String())
	}
	if cacheControl := response.Header().Get("Cache-Control"); !strings.Contains(cacheControl, "private") || !strings.Contains(cacheControl, "no-store") {
		t.Fatalf("protected workspace response is cacheable: %q", cacheControl)
	}

	logout := httptest.NewRequest(http.MethodPost, "http://example.com/api/auth/logout", nil)
	logout.Header.Set("Cookie", shadowedCookie)
	logout.Header.Set("Origin", "http://example.com")
	logout.Header.Set("X-VibeDock-CSRF", registered.status.CSRFToken)
	logoutResponse := httptest.NewRecorder()
	handler.ServeHTTP(logoutResponse, logout)
	if logoutResponse.Code != http.StatusOK {
		t.Fatalf("shadowed session logout returned %d: %s", logoutResponse.Code, logoutResponse.Body.String())
	}

	oldSession := performRequest(handler, http.MethodGet, "http://example.com/api/workspaces", "", registered.cookie, nil)
	if oldSession.Code != http.StatusUnauthorized {
		t.Fatalf("shadowed logout left the valid session active: %d", oldSession.Code)
	}
}

func TestRegisterAndLoginRequireSameOrigin(t *testing.T) {
	manager := newTestManager(t, filepath.Join(t.TempDir(), "admin.json"), Options{})
	handler := manager.Handler(http.NotFoundHandler())
	body := `{"username":"admin","password":"` + testPassword + `"}`

	for name, testCase := range map[string][2]string{
		"missing origin": {"", ""},
		"foreign origin": {"https://attacker.example", "cross-site"},
		"same-site only": {"http://example.com", "same-site"},
		"wrong scheme":   {"https://example.com", "same-origin"},
	} {
		t.Run(name, func(t *testing.T) {
			origin, fetchSite := testCase[0], testCase[1]
			response := performRequest(handler, http.MethodPost, "http://example.com/api/auth/register", body, nil, map[string]string{
				"Content-Type": "application/json", "Origin": origin, "Sec-Fetch-Site": fetchSite,
			})
			if response.Code != http.StatusForbidden {
				t.Fatalf("returned %d: %s", response.Code, response.Body.String())
			}
		})
	}

	wrongTokenBody := `{"username":"admin","password":"` + testPassword + `","setup_token":"wrong"}`
	wrongToken := performRequest(handler, http.MethodPost, "http://example.com/api/auth/register", wrongTokenBody, nil, map[string]string{
		"Content-Type": "application/json", "Origin": "http://example.com", "Sec-Fetch-Site": "same-origin",
	})
	if wrongToken.Code != http.StatusForbidden || !strings.Contains(wrongToken.Body.String(), "setup_token_invalid") {
		t.Fatalf("wrong setup token returned %d: %s", wrongToken.Code, wrongToken.Body.String())
	}
	validBody, err := json.Marshal(credentialRequest{Username: "admin", Password: testPassword, SetupToken: string(manager.bootstrapToken)})
	if err != nil {
		t.Fatal(err)
	}
	valid := performRequest(handler, http.MethodPost, "http://example.com/api/auth/register", string(validBody), nil, map[string]string{
		"Content-Type": "application/json", "Origin": "http://example.com", "Sec-Fetch-Site": "same-origin",
	})
	if valid.Code != http.StatusCreated {
		t.Fatalf("same-origin registration returned %d: %s", valid.Code, valid.Body.String())
	}
	duplicate := performRequest(handler, http.MethodPost, "http://example.com/api/auth/register", string(validBody), nil, map[string]string{
		"Content-Type": "application/json", "Origin": "http://example.com",
	})
	if duplicate.Code != http.StatusConflict {
		t.Fatalf("duplicate registration returned %d: %s", duplicate.Code, duplicate.Body.String())
	}
}

func TestSecureCookieAndSharedSessionMiddlewareBehindTrustedProxy(t *testing.T) {
	manager := newTestManager(t, filepath.Join(t.TempDir(), "admin.json"), Options{TrustProxyHeaders: true})
	control := manager.Handler(http.NotFoundHandler())
	requestBody, err := json.Marshal(credentialRequest{Username: "admin", Password: testPassword, SetupToken: string(manager.bootstrapToken)})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "http://vibedock.example/api/auth/register", bytes.NewReader(requestBody))
	request.Host = "vibedock.example"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://vibedock.example")
	request.Header.Set("X-Forwarded-Proto", "https")
	response := httptest.NewRecorder()
	control.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("proxied registration returned %d: %s", response.Code, response.Body.String())
	}
	cookie := response.Result().Cookies()[0]
	if !cookie.Secure {
		t.Fatal("HTTPS proxy registration did not set a Secure cookie")
	}

	tools := manager.SessionMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	toolResponse := performRequest(tools, http.MethodGet, "http://vibedock.example:8788/w/demo/code/", "", cookie, nil)
	if toolResponse.Code != http.StatusNoContent {
		t.Fatalf("shared tools session returned %d: %s", toolResponse.Code, toolResponse.Body.String())
	}

	websocket := performRequest(tools, http.MethodGet, "http://vibedock.example:8788/w/demo/socket", "", cookie, map[string]string{
		"Connection": "keep-alive, Upgrade", "Upgrade": "websocket", "Origin": "http://vibedock.example:8788",
	})
	if websocket.Code != http.StatusNoContent {
		t.Fatalf("same-origin tools WebSocket returned %d: %s", websocket.Code, websocket.Body.String())
	}
	foreignWebsocket := performRequest(tools, http.MethodGet, "http://vibedock.example:8788/w/demo/socket", "", cookie, map[string]string{
		"Connection": "Upgrade", "Upgrade": "websocket", "Origin": "http://vibedock.example:8787",
	})
	if foreignWebsocket.Code != http.StatusForbidden {
		t.Fatalf("foreign-origin tools WebSocket returned %d", foreignWebsocket.Code)
	}
}

func TestConcurrentRegistrationCreatesCredentialOnce(t *testing.T) {
	credentialPath := filepath.Join(t.TempDir(), "admin.json")
	first := newTestManager(t, credentialPath, Options{})
	second := newTestManager(t, credentialPath, Options{})
	if !bytes.Equal(first.bootstrapToken, second.bootstrapToken) {
		t.Fatal("managers did not load the same bootstrap token")
	}

	type result struct {
		username string
		status   int
		body     string
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	for _, item := range []struct {
		manager  *Manager
		username string
	}{{first, "admin-one"}, {second, "admin-two"}} {
		go func(manager *Manager, username string) {
			<-start
			body, _ := json.Marshal(credentialRequest{Username: username, Password: testPassword, SetupToken: string(manager.bootstrapToken)})
			response := performRequest(manager.Handler(http.NotFoundHandler()), http.MethodPost, "http://example.com/api/auth/register", string(body), nil, map[string]string{
				"Content-Type": "application/json", "Origin": "http://example.com", "Sec-Fetch-Site": "same-origin",
			})
			results <- result{username: username, status: response.Code, body: response.Body.String()}
		}(item.manager, item.username)
	}
	close(start)
	firstResult, secondResult := <-results, <-results
	statuses := map[int]int{firstResult.status: 1}
	statuses[secondResult.status]++
	if statuses[http.StatusCreated] != 1 || statuses[http.StatusConflict] != 1 {
		t.Fatalf("concurrent statuses: %#v (%s / %s)", statuses, firstResult.body, secondResult.body)
	}
	winner := firstResult.username
	if secondResult.status == http.StatusCreated {
		winner = secondResult.username
	}
	reloaded := newTestManager(t, credentialPath, Options{})
	if reloaded.credential == nil || reloaded.credential.Username != winner {
		t.Fatalf("stored username %q, want winning username %q", reloaded.credential.Username, winner)
	}
}

func TestConcurrentManagersShareAtomicBootstrapToken(t *testing.T) {
	for iteration := range 10 {
		credentialPath := filepath.Join(t.TempDir(), fmt.Sprintf("auth-%d", iteration), "admin.json")
		start := make(chan struct{})
		type result struct {
			manager *Manager
			err     error
		}
		results := make(chan result, 2)
		for range 2 {
			go func() {
				<-start
				manager, err := New(Options{CredentialFile: credentialPath, BcryptCost: bcrypt.MinCost})
				results <- result{manager: manager, err: err}
			}()
		}
		close(start)
		first, second := <-results, <-results
		if first.err != nil || second.err != nil {
			t.Fatalf("concurrent manager creation failed: %v / %v", first.err, second.err)
		}
		if !bytes.Equal(first.manager.bootstrapToken, second.manager.bootstrapToken) {
			t.Fatal("concurrent managers generated different bootstrap tokens")
		}
	}
}

func TestPersistCredentialIsCreateOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admin.json")
	firstHash, err := bcrypt.GenerateFromPassword([]byte(testPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	first := &diskCredential{Version: 1, Username: "first-admin", PasswordHash: string(firstHash), CreatedAt: time.Now().UTC()}
	second := &diskCredential{Version: 1, Username: "second-admin", PasswordHash: string(firstHash), CreatedAt: time.Now().UTC()}
	if err := persistCredential(path, first); err != nil {
		t.Fatal(err)
	}
	if err := persistCredential(path, second); !errors.Is(err, errCredentialExists) {
		t.Fatalf("second persist returned %v, want credential-exists", err)
	}
	stored, err := loadCredential(path)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Username != first.Username {
		t.Fatalf("second persist overwrote username with %q", stored.Username)
	}
}

func TestPasswordChecksDoNotHoldManagerLockAndAreLimited(t *testing.T) {
	manager := newTestManager(t, filepath.Join(t.TempDir(), "admin.json"), Options{MaxConcurrentPasswordChecks: 2})
	handler := manager.Handler(http.NotFoundHandler())
	register(t, manager, handler, "admin", testPassword)

	started := make(chan struct{}, 2)
	release := make(chan struct{})
	manager.comparePassword = func(_, _ []byte) error {
		started <- struct{}{}
		<-release
		return bcrypt.ErrMismatchedHashAndPassword
	}
	responses := make(chan *httptest.ResponseRecorder, 2)
	go func() { responses <- login(t, handler, "admin", "wrong-password") }()
	go func() { responses <- login(t, handler, "admin", "wrong-password") }()
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("password comparison did not start")
		}
	}

	statusDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		statusDone <- performRequest(handler, http.MethodGet, "http://example.com/api/auth/status", "", nil, nil)
	}()
	select {
	case response := <-statusDone:
		if response.Code != http.StatusOK {
			t.Fatalf("status while bcrypt active returned %d", response.Code)
		}
	case <-time.After(time.Second):
		t.Fatal("status blocked behind password comparison")
	}

	busyDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { busyDone <- login(t, handler, "admin", "wrong-password") }()
	select {
	case response := <-busyDone:
		if response.Code != http.StatusTooManyRequests || !strings.Contains(response.Body.String(), "login_busy") {
			t.Fatalf("busy login returned %d: %s", response.Code, response.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("busy login did not fail promptly")
	}
	close(release)
	for range 2 {
		if response := <-responses; response.Code != http.StatusUnauthorized {
			t.Fatalf("released login returned %d: %s", response.Code, response.Body.String())
		}
	}
}

func TestSessionAndFailureMapsAreBounded(t *testing.T) {
	manager := newTestManager(t, filepath.Join(t.TempDir(), "admin.json"), Options{
		MaxSessions: 2, MaxFailureClients: 2, MaxFailures: 10,
	})
	handler := manager.Handler(http.NotFoundHandler())
	register(t, manager, handler, "admin", testPassword)
	for range 5 {
		if response := login(t, handler, "admin", testPassword); response.Code != http.StatusOK {
			t.Fatalf("login returned %d: %s", response.Code, response.Body.String())
		}
	}
	manager.mu.Lock()
	if len(manager.sessions) > 2 {
		t.Fatalf("session map grew to %d", len(manager.sessions))
	}
	manager.mu.Unlock()

	for index := range 8 {
		response := loginFrom(t, handler, "admin", "wrong-password", fmt.Sprintf("192.0.2.%d:1234", index+1))
		if response.Code != http.StatusUnauthorized && response.Code != http.StatusTooManyRequests {
			t.Fatalf("failed login returned %d: %s", response.Code, response.Body.String())
		}
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if len(manager.failures) > 2 {
		t.Fatalf("failure map grew to %d", len(manager.failures))
	}
	for key, attempts := range manager.failures {
		if len(attempts) > manager.maxFailures {
			t.Fatalf("failure bucket %q grew to %d", key, len(attempts))
		}
	}
}

func TestLogoutCancelsAuthenticatedRequestContext(t *testing.T) {
	manager := newTestManager(t, filepath.Join(t.TempDir(), "admin.json"), Options{})
	requestStarted := make(chan struct{})
	requestCanceled := make(chan struct{})
	app := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(requestStarted)
		<-r.Context().Done()
		close(requestCanceled)
	})
	handler := manager.Handler(app)
	registered := register(t, manager, handler, "admin", testPassword)
	go func() {
		_ = performRequest(handler, http.MethodGet, "http://example.com/w/demo/socket", "", registered.cookie, nil)
	}()
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("protected request did not start")
	}
	logout := performRequest(handler, http.MethodPost, "http://example.com/api/auth/logout", "", registered.cookie, map[string]string{
		"Origin": "http://example.com", "X-VibeDock-CSRF": registered.status.CSRFToken,
	})
	if logout.Code != http.StatusOK {
		t.Fatalf("logout returned %d: %s", logout.Code, logout.Body.String())
	}
	select {
	case <-requestCanceled:
	case <-time.After(time.Second):
		t.Fatal("logout did not cancel protected request")
	}
}

func TestSessionEvictionAndExpiryCancelAuthenticatedContexts(t *testing.T) {
	t.Run("capacity eviction", func(t *testing.T) {
		manager := newTestManager(t, filepath.Join(t.TempDir(), "admin.json"), Options{MaxSessions: 1})
		requestStarted := make(chan struct{})
		requestCanceled := make(chan struct{})
		app := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			close(requestStarted)
			<-r.Context().Done()
			close(requestCanceled)
		})
		handler := manager.Handler(app)
		registered := register(t, manager, handler, "admin", testPassword)
		go func() {
			_ = performRequest(handler, http.MethodGet, "http://example.com/w/demo/socket", "", registered.cookie, nil)
		}()
		select {
		case <-requestStarted:
		case <-time.After(time.Second):
			t.Fatal("protected request did not start")
		}
		if response := login(t, handler, "admin", testPassword); response.Code != http.StatusOK {
			t.Fatalf("login returned %d: %s", response.Code, response.Body.String())
		}
		select {
		case <-requestCanceled:
		case <-time.After(time.Second):
			t.Fatal("session eviction did not cancel protected request")
		}
	})

	t.Run("absolute expiry", func(t *testing.T) {
		manager := newTestManager(t, filepath.Join(t.TempDir(), "admin.json"), Options{SessionTTL: 50 * time.Millisecond})
		requestCanceled := make(chan struct{})
		app := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
			close(requestCanceled)
		})
		handler := manager.Handler(app)
		registered := register(t, manager, handler, "admin", testPassword)
		go func() {
			_ = performRequest(handler, http.MethodGet, "http://example.com/w/demo/socket", "", registered.cookie, nil)
		}()
		select {
		case <-requestCanceled:
		case <-time.After(time.Second):
			t.Fatal("session expiry did not cancel protected request")
		}
	})
}

type registrationResult struct {
	response *httptest.ResponseRecorder
	cookie   *http.Cookie
	status   statusResponse
}

func newTestManager(t *testing.T, path string, overrides Options) *Manager {
	t.Helper()
	overrides.CredentialFile = path
	overrides.BcryptCost = bcrypt.MinCost
	manager, err := New(overrides)
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func register(t *testing.T, manager *Manager, handler http.Handler, username, password string) registrationResult {
	t.Helper()
	body, err := json.Marshal(credentialRequest{Username: username, Password: password, SetupToken: string(manager.bootstrapToken)})
	if err != nil {
		t.Fatal(err)
	}
	response := performRequest(handler, http.MethodPost, "http://example.com/api/auth/register", string(body), nil, map[string]string{
		"Content-Type": "application/json", "Origin": "http://example.com", "Sec-Fetch-Site": "same-origin",
	})
	cookies := response.Result().Cookies()
	if response.Code == http.StatusCreated && len(cookies) != 1 {
		t.Fatalf("registration returned %d cookies", len(cookies))
	}
	result := registrationResult{response: response, status: statusJSON(t, response)}
	if len(cookies) > 0 {
		result.cookie = cookies[0]
	}
	return result
}

func login(t *testing.T, handler http.Handler, username, password string) *httptest.ResponseRecorder {
	return loginFrom(t, handler, username, password, "192.0.2.10:1234")
}

func loginFrom(t *testing.T, handler http.Handler, username, password, remoteAddr string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(credentialRequest{Username: username, Password: password})
	if err != nil {
		t.Fatal(err)
	}
	return performRequestFrom(handler, http.MethodPost, "http://example.com/api/auth/login", string(body), nil, map[string]string{
		"Content-Type": "application/json", "Origin": "http://example.com", "Sec-Fetch-Site": "same-origin",
	}, remoteAddr)
}

func performRequest(handler http.Handler, method, target, body string, cookie *http.Cookie, headers map[string]string) *httptest.ResponseRecorder {
	return performRequestFrom(handler, method, target, body, cookie, headers, "192.0.2.10:1234")
}

func performRequestFrom(handler http.Handler, method, target, body string, cookie *http.Cookie, headers map[string]string, remoteAddr string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.RemoteAddr = remoteAddr
	if cookie != nil {
		request.AddCookie(cookie)
	}
	for key, value := range headers {
		if value != "" {
			request.Header.Set(key, value)
		}
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func statusJSON(t *testing.T, response *httptest.ResponseRecorder) statusResponse {
	t.Helper()
	var value statusResponse
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
		t.Fatalf("decode status response: %v (%q)", err, response.Body.String())
	}
	return value
}
