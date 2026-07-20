package app

import (
	"bytes"
	"context"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"vibedock/internal/model"
	"vibedock/internal/store"
)

type fakeAuthRuntime struct {
	mu          sync.Mutex
	started     []model.AuthStartRequest
	cancelled   []model.AuthAttemptRef
	attempts    map[string]model.AuthRuntimeAttempt
	statuses    map[string]model.AuthStatusResult
	logout      model.AuthLogoutResult
	cleanups    []model.AuthCredentialCleanup
	committed   []model.AuthCredentialCleanup
	rolledBack  []model.AuthCredentialCleanup
	prepareErr  error
	commitErr   error
	rollbackErr error
}

func (f *fakeAuthRuntime) StartAuth(_ context.Context, request model.AuthStartRequest) (model.AuthRuntimeAttempt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	attempt := model.AuthRuntimeAttempt{
		AuthAttemptRef: request.AuthAttemptRef,
		State:          model.AuthAttemptAwaitingUser,
		ExpiresAt:      request.ExpiresAt,
		PollAfter:      time.Second,
		Message:        "等待官方登录",
	}
	if request.Provider == model.AuthProviderCodex {
		attempt.VerificationURL = "https://auth.openai.com/deviceauth"
		attempt.UserCode = "ABCD-EFGH"
	} else {
		attempt.TerminalAvailable = true
	}
	f.started = append(f.started, request)
	if f.attempts == nil {
		f.attempts = make(map[string]model.AuthRuntimeAttempt)
	}
	f.attempts[request.AttemptID] = attempt
	return attempt, nil
}

func (f *fakeAuthRuntime) InspectAuth(_ context.Context, reference model.AuthAttemptRef) (model.AuthRuntimeAttempt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	attempt, ok := f.attempts[reference.AttemptID]
	if !ok {
		return model.AuthRuntimeAttempt{}, os.ErrNotExist
	}
	return attempt, nil
}

func (f *fakeAuthRuntime) CancelAuth(_ context.Context, reference model.AuthAttemptRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelled = append(f.cancelled, reference)
	if attempt, ok := f.attempts[reference.AttemptID]; ok {
		attempt.State = model.AuthAttemptCancelled
		attempt.VerificationURL = ""
		attempt.UserCode = ""
		attempt.TerminalAvailable = false
		f.attempts[reference.AttemptID] = attempt
	}
	return nil
}

func (f *fakeAuthRuntime) AuthStatus(_ context.Context, request model.AuthStatusRequest) (model.AuthStatusResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if result, ok := f.statuses[request.AccountID]; ok {
		return result, nil
	}
	return model.AuthStatusResult{Status: model.AuthStatusUnauthenticated, CheckedAt: time.Now().UTC()}, nil
}

func (f *fakeAuthRuntime) LogoutAuth(context.Context, model.AuthLogoutRequest) (model.AuthLogoutResult, error) {
	if !f.logout.LocalCredentialsRemoved {
		return model.AuthLogoutResult{LocalCredentialsRemoved: true}, nil
	}
	return f.logout, nil
}

func (f *fakeAuthRuntime) AuthTerminal(context.Context, model.AuthAttemptRef) (model.AuthTerminalTarget, error) {
	return model.AuthTerminalTarget{URL: "http://127.0.0.1:7681", Username: "vibe", Password: "internal-only"}, nil
}

func (f *fakeAuthRuntime) PrepareAuthCredentialCleanup(_ context.Context, accountID string) (model.AuthCredentialCleanup, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.prepareErr != nil {
		return model.AuthCredentialCleanup{}, f.prepareErr
	}
	cleanup := model.AuthCredentialCleanup{AccountID: accountID, CleanupID: "cleanup00000000000000000001"}
	f.cleanups = append(f.cleanups, cleanup)
	return cleanup, nil
}

func (f *fakeAuthRuntime) CommitAuthCredentialCleanup(_ context.Context, cleanup model.AuthCredentialCleanup) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.committed = append(f.committed, cleanup)
	return f.commitErr
}

func (f *fakeAuthRuntime) RollbackAuthCredentialCleanup(_ context.Context, cleanup model.AuthCredentialCleanup) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rolledBack = append(f.rolledBack, cleanup)
	return f.rollbackErr
}

func (f *fakeAuthRuntime) ReconcileAuth(context.Context, time.Time) error { return nil }

func newAuthTestServer(t *testing.T, runtime *fakeAuthRuntime, accounts ...model.AccountProfile) (*Server, http.Handler) {
	t.Helper()
	st, err := store.New(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, account := range accounts {
		if err := st.PutAccount(account); err != nil {
			t.Fatal(err)
		}
	}
	web := fs.FS(fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("ok")}})
	server := NewServerWithAuth(st, &fakeRuntime{}, runtime, nil, web, slog.Default(), testKeyring(t), AccountAuthOptions{Enabled: true, TTL: 15 * time.Minute, MaxConcurrent: 3})
	return server, server.Handler()
}

func authAccount(id, agent string) model.AccountProfile {
	now := time.Now().UTC()
	return model.AccountProfile{ID: id, Name: agent, Agent: agent, AuthType: "login", CreatedAt: now, UpdatedAt: now}
}

func authMutationRequest(method, path string) *http.Request {
	request := httptest.NewRequest(method, path, bytes.NewBufferString("{}"))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-VibeDock-Request", "account-auth")
	request.Header.Set("Origin", "http://example.com")
	return request
}

func TestAccountLoginAttemptAPIIsIdempotentAndNoStore(t *testing.T) {
	runtime := &fakeAuthRuntime{}
	_, handler := newAuthTestServer(t, runtime, authAccount("acct12345678", "codex"))

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, authMutationRequest(http.MethodPost, "http://example.com/api/accounts/acct12345678/login-attempts"))
	if first.Code != http.StatusCreated {
		t.Fatalf("first start status = %d, body = %s", first.Code, first.Body.String())
	}
	if first.Header().Get("Cache-Control") != "no-store, private" {
		t.Fatalf("missing no-store header: %q", first.Header().Get("Cache-Control"))
	}

	second := httptest.NewRecorder()
	handler.ServeHTTP(second, authMutationRequest(http.MethodPost, "http://example.com/api/accounts/acct12345678/login-attempts"))
	if second.Code != http.StatusOK {
		t.Fatalf("second start status = %d, body = %s", second.Code, second.Body.String())
	}
	runtime.mu.Lock()
	started := len(runtime.started)
	runtime.mu.Unlock()
	if started != 1 {
		t.Fatalf("StartAuth called %d times, want 1", started)
	}
	if !bytes.Contains(first.Body.Bytes(), []byte("ABCD-EFGH")) || !bytes.Contains(first.Body.Bytes(), []byte("https://auth.openai.com/deviceauth")) {
		t.Fatalf("active attempt omitted device flow data: %s", first.Body.String())
	}
}

func TestAccountLoginAttemptRequiresMutationHeaders(t *testing.T) {
	runtime := &fakeAuthRuntime{}
	_, handler := newAuthTestServer(t, runtime, authAccount("acct12345678", "codex"))
	request := httptest.NewRequest(http.MethodPost, "http://example.com/api/accounts/acct12345678/login-attempts", bytes.NewBufferString("{}"))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", response.Code)
	}
	if response.Header().Get("Cache-Control") != "no-store, private" {
		t.Fatal("auth error response must not be cached")
	}
}

func TestAccountLoginAttemptCapacityIsThree(t *testing.T) {
	runtime := &fakeAuthRuntime{}
	accounts := []model.AccountProfile{
		authAccount("acct00000001", "codex"),
		authAccount("acct00000002", "claude"),
		authAccount("acct00000003", "codex"),
		authAccount("acct00000004", "claude"),
	}
	_, handler := newAuthTestServer(t, runtime, accounts...)
	for index, account := range accounts {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, authMutationRequest(http.MethodPost, "http://example.com/api/accounts/"+account.ID+"/login-attempts"))
		if index < 3 && response.Code != http.StatusCreated {
			t.Fatalf("attempt %d status = %d, body = %s", index, response.Code, response.Body.String())
		}
		if index == 3 && response.Code != http.StatusTooManyRequests {
			t.Fatalf("fourth status = %d, body = %s", response.Code, response.Body.String())
		}
	}
}

func TestAccountLoginStatusReportsActiveAttempt(t *testing.T) {
	runtime := &fakeAuthRuntime{}
	_, handler := newAuthTestServer(t, runtime, authAccount("acct12345678", "claude"))
	start := httptest.NewRecorder()
	handler.ServeHTTP(start, authMutationRequest(http.MethodPost, "http://example.com/api/accounts/acct12345678/login-attempts"))
	status := httptest.NewRecorder()
	handler.ServeHTTP(status, httptest.NewRequest(http.MethodGet, "http://example.com/api/accounts/acct12345678/auth-status", nil))
	if status.Code != http.StatusOK || !bytes.Contains(status.Body.Bytes(), []byte(`"state":"login_in_progress"`)) || !bytes.Contains(status.Body.Bytes(), []byte(`"terminal_url"`)) {
		t.Fatalf("unexpected active status: %d %s", status.Code, status.Body.String())
	}
}

func TestAccountLoginAttemptCancelClearsSensitiveData(t *testing.T) {
	runtime := &fakeAuthRuntime{}
	_, handler := newAuthTestServer(t, runtime, authAccount("acct12345678", "codex"))
	start := httptest.NewRecorder()
	handler.ServeHTTP(start, authMutationRequest(http.MethodPost, "http://example.com/api/accounts/acct12345678/login-attempts"))
	body := start.Body.String()
	marker := `"id":"`
	position := bytes.Index([]byte(body), []byte(marker))
	if position < 0 || len(body) < position+len(marker)+24 {
		t.Fatalf("missing attempt ID: %s", body)
	}
	attemptID := body[position+len(marker) : position+len(marker)+24]

	cancel := httptest.NewRecorder()
	handler.ServeHTTP(cancel, authMutationRequest(http.MethodDelete, "http://example.com/api/accounts/acct12345678/login-attempts/"+attemptID))
	if cancel.Code != http.StatusNoContent {
		t.Fatalf("cancel status = %d, body = %s", cancel.Code, cancel.Body.String())
	}
	inspect := httptest.NewRecorder()
	handler.ServeHTTP(inspect, httptest.NewRequest(http.MethodGet, "http://example.com/api/accounts/acct12345678/login-attempts/"+attemptID, nil))
	if inspect.Code != http.StatusOK || bytes.Contains(inspect.Body.Bytes(), []byte("ABCD-EFGH")) || bytes.Contains(inspect.Body.Bytes(), []byte("auth.openai.com")) {
		t.Fatalf("terminal attempt leaked transient data: %d %s", inspect.Code, inspect.Body.String())
	}
}

func TestAccountLoginAttemptExpiryCancelsRuntime(t *testing.T) {
	runtime := &fakeAuthRuntime{}
	manager := newAccountAuthManager(runtime, AccountAuthOptions{Enabled: true, TTL: 15 * time.Minute, MaxConcurrent: 3})
	attempt, _, err := manager.start(context.Background(), "acct12345678", model.AuthProviderCodex)
	if err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	manager.attempts[attempt.AttemptID].view.ExpiresAt = time.Now().Add(-time.Second)
	manager.mu.Unlock()
	current := manager.current("acct12345678")
	if current == nil || current.State != model.AuthAttemptExpired || current.VerificationURL != "" || current.UserCode != "" {
		t.Fatalf("unexpected expired attempt: %#v", current)
	}
	runtime.mu.Lock()
	cancelled := len(runtime.cancelled)
	runtime.mu.Unlock()
	if cancelled != 1 {
		t.Fatalf("CancelAuth called %d times, want 1", cancelled)
	}
}
