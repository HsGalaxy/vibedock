package app

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"vibedock/internal/model"
	"vibedock/internal/store"
)

type fakeAuthRuntime struct {
	mu                 sync.Mutex
	started            []model.AuthStartRequest
	completed          []model.AuthCompleteRequest
	cancelled          []model.AuthAttemptRef
	attempts           map[string]model.AuthRuntimeAttempt
	statuses           map[string]model.AuthStatusResult
	logout             model.AuthLogoutResult
	completeErr        error
	completeContextErr error
	cleanups           []model.AuthCredentialCleanup
	committed          []model.AuthCredentialCleanup
	rolledBack         []model.AuthCredentialCleanup
	prepareErr         error
	commitErr          error
	rollbackErr        error
}

func (f *fakeAuthRuntime) StartAuth(_ context.Context, request model.AuthStartRequest) (model.AuthRuntimeAttempt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	attempt := model.AuthRuntimeAttempt{
		AuthAttemptRef: request.AuthAttemptRef,
		State:          model.AuthAttemptAwaitingUser,
		ExpiresAt:      request.ExpiresAt,
		PollAfter:      time.Second,
		Message:        "等待官方授权",
	}
	if request.Provider == model.AuthProviderCodex {
		attempt.VerificationURL = "https://auth.openai.com/oauth/authorize?client_id=test"
	} else {
		attempt.VerificationURL = "https://claude.com/cai/oauth/authorize?client_id=test"
	}
	f.started = append(f.started, request)
	if f.attempts == nil {
		f.attempts = make(map[string]model.AuthRuntimeAttempt)
	}
	f.attempts[request.AttemptID] = attempt
	return attempt, nil
}

func (f *fakeAuthRuntime) CompleteAuth(ctx context.Context, request model.AuthCompleteRequest) (model.AuthRuntimeAttempt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completeContextErr = ctx.Err()
	if f.completeContextErr != nil {
		return model.AuthRuntimeAttempt{}, f.completeContextErr
	}
	f.completed = append(f.completed, request)
	attempt, ok := f.attempts[request.AttemptID]
	if !ok {
		return model.AuthRuntimeAttempt{}, os.ErrNotExist
	}
	attempt.State = model.AuthAttemptCompleted
	attempt.VerificationURL = ""
	attempt.UserCode = ""
	attempt.TerminalAvailable = false
	attempt.Message = "授权完成"
	f.attempts[request.AttemptID] = attempt
	if f.completeErr != nil {
		return model.AuthRuntimeAttempt{}, f.completeErr
	}
	return attempt, nil
}

func (f *fakeAuthRuntime) InspectAuth(ctx context.Context, reference model.AuthAttemptRef) (model.AuthRuntimeAttempt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return model.AuthRuntimeAttempt{}, err
	}
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
	return authMutationJSONRequest(method, path, "{}")
}

func authMutationJSONRequest(method, path, body string) *http.Request {
	request := httptest.NewRequest(method, path, bytes.NewBufferString(body))
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
	if !bytes.Contains(first.Body.Bytes(), []byte(`"flow":"authorization_code"`)) || !bytes.Contains(first.Body.Bytes(), []byte("https://auth.openai.com/oauth/authorize")) || bytes.Contains(first.Body.Bytes(), []byte("user_code")) {
		t.Fatalf("active attempt omitted authorization flow data: %s", first.Body.String())
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
	if status.Code != http.StatusOK || !bytes.Contains(status.Body.Bytes(), []byte(`"state":"login_in_progress"`)) || !bytes.Contains(status.Body.Bytes(), []byte("https://claude.com/cai/oauth/authorize")) {
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
	if inspect.Code != http.StatusOK || bytes.Contains(inspect.Body.Bytes(), []byte("auth.openai.com")) {
		t.Fatalf("terminal attempt leaked transient data: %d %s", inspect.Code, inspect.Body.String())
	}
}

func TestAccountLoginCompleteAcceptsAuthorizationResponse(t *testing.T) {
	runtime := &fakeAuthRuntime{}
	_, handler := newAuthTestServer(t, runtime, authAccount("acct12345678", "codex"))
	start := httptest.NewRecorder()
	handler.ServeHTTP(start, authMutationRequest(http.MethodPost, "http://example.com/api/accounts/acct12345678/login-attempts"))
	marker := `"id":"`
	position := strings.Index(start.Body.String(), marker)
	if position < 0 {
		t.Fatalf("missing attempt ID: %s", start.Body.String())
	}
	attemptID := start.Body.String()[position+len(marker) : position+len(marker)+24]
	callback := "http://localhost:1455/auth/callback?code=provider-code&state=oauth-state"
	complete := httptest.NewRecorder()
	request := authMutationJSONRequest(http.MethodPost, "http://example.com/api/accounts/acct12345678/login-attempts/"+attemptID+"/complete", `{"authorization_response":"`+callback+`"}`)
	handler.ServeHTTP(complete, request)
	if complete.Code != http.StatusOK || !bytes.Contains(complete.Body.Bytes(), []byte(`"state":"completed"`)) || bytes.Contains(complete.Body.Bytes(), []byte("verification_url")) {
		t.Fatalf("complete status = %d, body = %s", complete.Code, complete.Body.String())
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if len(runtime.completed) != 1 || runtime.completed[0].AuthorizationResponse != callback {
		t.Fatalf("unexpected completion request: %#v", runtime.completed)
	}
}

func TestAccountLoginCompleteRecoversAfterRuntimeTimeout(t *testing.T) {
	runtime := &fakeAuthRuntime{completeErr: errors.New("timeout after credential commit")}
	_, handler := newAuthTestServer(t, runtime, authAccount("acct12345678", "codex"))
	start := httptest.NewRecorder()
	handler.ServeHTTP(start, authMutationRequest(http.MethodPost, "http://example.com/api/accounts/acct12345678/login-attempts"))
	marker := `"id":"`
	position := strings.Index(start.Body.String(), marker)
	attemptID := start.Body.String()[position+len(marker) : position+len(marker)+24]
	complete := httptest.NewRecorder()
	handler.ServeHTTP(complete, authMutationJSONRequest(http.MethodPost, "http://example.com/api/accounts/acct12345678/login-attempts/"+attemptID+"/complete", `{"authorization_response":"http://localhost:1455/auth/callback?code=ok&state=ok"}`))
	if complete.Code != http.StatusOK || !bytes.Contains(complete.Body.Bytes(), []byte(`"state":"completed"`)) {
		t.Fatalf("committed credentials were reported as timeout: %d %s", complete.Code, complete.Body.String())
	}
}

func TestAccountLoginCompleteSurvivesClientCancellation(t *testing.T) {
	runtime := &fakeAuthRuntime{}
	manager := newAccountAuthManager(runtime, AccountAuthOptions{Enabled: true, TTL: 15 * time.Minute, MaxConcurrent: 3})
	attempt, _, err := manager.start(context.Background(), "acct12345678", model.AuthProviderCodex)
	if err != nil {
		t.Fatal(err)
	}
	requestContext, cancel := context.WithCancel(context.Background())
	cancel()
	completed, err := manager.complete(requestContext, "acct12345678", attempt.AttemptID, model.AuthProviderCodex, "http://localhost:1455/auth/callback?code=ok&state=ok")
	if err != nil {
		t.Fatal(err)
	}
	if completed.State != model.AuthAttemptCompleted {
		t.Fatalf("completion was lost after client cancellation: %#v", completed)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.completeContextErr != nil {
		t.Fatalf("runtime received cancelled request context: %v", runtime.completeContextErr)
	}
}

func TestAccountAuthClosePreservesRecoverableAttempt(t *testing.T) {
	runtime := &fakeAuthRuntime{}
	manager := newAccountAuthManager(runtime, AccountAuthOptions{Enabled: true, TTL: 15 * time.Minute, MaxConcurrent: 3})
	attempt, _, err := manager.start(context.Background(), "acct12345678", model.AuthProviderClaude)
	if err != nil {
		t.Fatal(err)
	}
	manager.Close(context.Background())
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if len(runtime.cancelled) != 0 {
		t.Fatalf("shutdown cancelled persisted authorization: %#v", runtime.cancelled)
	}
	if runtime.attempts[attempt.AttemptID].State != model.AuthAttemptAwaitingUser {
		t.Fatalf("shutdown changed recoverable authorization: %#v", runtime.attempts[attempt.AttemptID])
	}
}

func TestAccountLoginAttemptCanBeRecoveredFromRuntime(t *testing.T) {
	const attemptID = "attempt12345678901234567"
	runtime := &fakeAuthRuntime{attempts: map[string]model.AuthRuntimeAttempt{
		attemptID: {
			AuthAttemptRef:  model.AuthAttemptRef{AccountID: "acct12345678", AttemptID: attemptID, Provider: model.AuthProviderClaude},
			State:           model.AuthAttemptAwaitingUser,
			VerificationURL: "https://claude.com/cai/oauth/authorize?client_id=test",
			ExpiresAt:       time.Now().UTC().Add(10 * time.Minute),
			PollAfter:       2 * time.Second,
		},
	}}
	_, handler := newAuthTestServer(t, runtime, authAccount("acct12345678", "claude"))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://example.com/api/accounts/acct12345678/login-attempts/"+attemptID, nil))
	if response.Code != http.StatusOK || !bytes.Contains(response.Body.Bytes(), []byte("https://claude.com/cai/oauth/authorize")) {
		t.Fatalf("persisted attempt was not recovered: %d %s", response.Code, response.Body.String())
	}
}

func TestAccountLoginAttemptCompletionWinsOverCachedExpiry(t *testing.T) {
	runtime := &fakeAuthRuntime{}
	manager := newAccountAuthManager(runtime, AccountAuthOptions{Enabled: true, TTL: 15 * time.Minute, MaxConcurrent: 3})
	attempt, _, err := manager.start(context.Background(), "acct12345678", model.AuthProviderCodex)
	if err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	manager.attempts[attempt.AttemptID].view.ExpiresAt = time.Now().Add(-time.Second)
	manager.mu.Unlock()
	runtime.mu.Lock()
	completed := runtime.attempts[attempt.AttemptID]
	completed.State = model.AuthAttemptCompleted
	completed.VerificationURL = ""
	runtime.attempts[attempt.AttemptID] = completed
	runtime.mu.Unlock()
	fresh, err := manager.inspect(context.Background(), "acct12345678", attempt.AttemptID, model.AuthProviderCodex)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.State != model.AuthAttemptCompleted {
		t.Fatalf("persisted completion was overwritten: %#v", fresh)
	}
}
