package app

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"vibedock/internal/model"
)

const (
	defaultAuthTTL       = 15 * time.Minute
	completedAuthKeepTTL = 10 * time.Minute
	defaultAuthCapacity  = 3
	authResponseLimit    = 8 << 10
	authCompletionTTL    = 80 * time.Second
)

type AccountAuthOptions struct {
	Enabled                 bool
	ControlPlaneAuthEnabled bool
	TTL                     time.Duration
	MaxConcurrent           int
}

type accountAuthAttempt struct {
	view      model.AuthRuntimeAttempt
	updatedAt time.Time
	runtimeMu sync.Mutex
}

type accountAuthManager struct {
	runtime          model.AuthRuntime
	ttl              time.Duration
	maxConcurrent    int
	mu               sync.Mutex
	attempts         map[string]*accountAuthAttempt
	attemptByAccount map[string]string
}

type loginAttemptView struct {
	ID              string                 `json:"id"`
	AccountID       string                 `json:"account_id"`
	Provider        model.AuthProvider     `json:"provider"`
	Flow            string                 `json:"flow"`
	State           model.AuthAttemptState `json:"state"`
	CreatedAt       time.Time              `json:"created_at"`
	ExpiresAt       time.Time              `json:"expires_at"`
	PollAfterMS     int64                  `json:"poll_after_ms"`
	VerificationURL string                 `json:"verification_url,omitempty"`
	Message         string                 `json:"message,omitempty"`
	ErrorCode       string                 `json:"error_code,omitempty"`
}

type accountAuthStatusView struct {
	AccountID string             `json:"account_id"`
	Provider  model.AuthProvider `json:"provider"`
	State     model.AuthStatus   `json:"state"`
	CheckedAt time.Time          `json:"checked_at"`
	Message   string             `json:"message,omitempty"`
	Attempt   *loginAttemptView  `json:"attempt,omitempty"`
}

func newAccountAuthManager(runtime model.AuthRuntime, options AccountAuthOptions) *accountAuthManager {
	if runtime == nil || !options.Enabled {
		return nil
	}
	ttl := options.TTL
	if ttl <= 0 {
		ttl = defaultAuthTTL
	}
	if ttl < 5*time.Minute {
		ttl = 5 * time.Minute
	}
	if ttl > 30*time.Minute {
		ttl = 30 * time.Minute
	}
	capacity := options.MaxConcurrent
	if capacity <= 0 {
		capacity = defaultAuthCapacity
	}
	if capacity > 3 {
		capacity = 3
	}
	return &accountAuthManager{
		runtime: runtime, ttl: ttl, maxConcurrent: capacity,
		attempts: make(map[string]*accountAuthAttempt), attemptByAccount: make(map[string]string),
	}
}

func providerForAccount(account model.AccountProfile) (model.AuthProvider, error) {
	if account.AuthType != "login" {
		return "", errors.New("只有网页登录账号支持此操作")
	}
	switch account.Agent {
	case "codex":
		return model.AuthProviderCodex, nil
	case "claude":
		return model.AuthProviderClaude, nil
	default:
		return "", errors.New("当前账号不支持网页登录")
	}
}

func (s *Server) authAvailable() bool {
	return s.auth != nil
}

func setAuthResponseHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store, private")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

func validateAuthMutation(r *http.Request) error {
	if r.Header.Get("X-VibeDock-Request") != "account-auth" {
		return errors.New("缺少账号登录请求标记")
	}
	if site := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))); site != "" && site != "same-origin" && site != "none" {
		return errors.New("拒绝跨站账号登录请求")
	}
	if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" {
		parsed, err := url.Parse(origin)
		if err != nil || parsed.Host != r.Host || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return errors.New("账号登录请求 Origin 不匹配")
		}
	}
	if r.Method == http.MethodPost {
		mediaType := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]))
		if mediaType != "application/json" {
			return errors.New("账号登录请求必须使用 JSON")
		}
	}
	return nil
}

func (s *Server) handleAccountAuthStatus(w http.ResponseWriter, r *http.Request) {
	setAuthResponseHeaders(w)
	account, ok := s.accountFromRequest(w, r)
	if !ok {
		return
	}
	provider, err := providerForAccount(account)
	if err != nil {
		writeError(w, http.StatusBadRequest, "账号不支持网页登录", err)
		return
	}
	if s.auth == nil {
		writeJSON(w, http.StatusServiceUnavailable, accountAuthStatusView{AccountID: account.ID, Provider: provider, State: model.AuthStatusUnavailable, CheckedAt: time.Now().UTC(), Message: "登录助手尚未启用"})
		return
	}
	if attempt := s.auth.current(account.ID); attempt != nil {
		if fresh, inspectErr := s.auth.inspect(r.Context(), account.ID, attempt.AttemptID, provider); inspectErr == nil {
			attempt = &fresh
		}
		if !attempt.State.Terminal() {
			view := s.auth.publicAttempt(*attempt)
			writeJSON(w, http.StatusOK, accountAuthStatusView{AccountID: account.ID, Provider: provider, State: model.AuthStatusLoginInProgress, CheckedAt: time.Now().UTC(), Message: attempt.Message, Attempt: &view})
			return
		}
	}
	result, err := s.auth.runtime.AuthStatus(r.Context(), model.AuthStatusRequest{AccountID: account.ID, Provider: provider})
	if err != nil {
		writeJSON(w, http.StatusOK, accountAuthStatusView{AccountID: account.ID, Provider: provider, State: model.AuthStatusUnavailable, CheckedAt: time.Now().UTC(), Message: "暂时无法检查本地登录凭据"})
		return
	}
	writeJSON(w, http.StatusOK, accountAuthStatusView{AccountID: account.ID, Provider: provider, State: result.Status, CheckedAt: result.CheckedAt, Message: result.Message})
}

func (s *Server) handleStartAccountLogin(w http.ResponseWriter, r *http.Request) {
	setAuthResponseHeaders(w)
	s.accountOperationMu.Lock()
	defer s.accountOperationMu.Unlock()
	if err := validateAuthMutation(r); err != nil {
		writeError(w, http.StatusForbidden, "账号登录请求被拒绝", err)
		return
	}
	account, ok := s.accountFromRequest(w, r)
	if !ok {
		return
	}
	provider, err := providerForAccount(account)
	if err != nil {
		writeError(w, http.StatusBadRequest, "账号不支持网页登录", err)
		return
	}
	if s.auth == nil {
		writeError(w, http.StatusServiceUnavailable, "登录助手尚未启用", nil)
		return
	}
	if count := s.activeAccountReferenceCount(account.ID); count > 0 {
		writeError(w, http.StatusConflict, fmt.Sprintf("仍有 %d 个运行中的 Session 使用该账号，请先停止对应 Workspace", count), nil)
		return
	}
	attempt, existing, err := s.auth.start(r.Context(), account.ID, provider)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, errAuthCapacity) {
			status = http.StatusTooManyRequests
		}
		writeError(w, status, "无法开始网页登录", err)
		return
	}
	status := http.StatusCreated
	if existing {
		status = http.StatusOK
	}
	writeJSON(w, status, s.auth.publicAttempt(attempt))
}

func (s *Server) handleAccountLoginAttempt(w http.ResponseWriter, r *http.Request) {
	setAuthResponseHeaders(w)
	account, ok := s.accountFromRequest(w, r)
	if !ok {
		return
	}
	attemptID := strings.TrimSpace(r.PathValue("attempt"))
	if !validAuthAttemptID(attemptID) {
		writeError(w, http.StatusBadRequest, "登录尝试 ID 不合法", nil)
		return
	}
	if s.auth == nil {
		writeError(w, http.StatusServiceUnavailable, "登录助手尚未启用", nil)
		return
	}
	provider, err := providerForAccount(account)
	if err != nil {
		writeError(w, http.StatusBadRequest, "账号不支持网页登录", err)
		return
	}
	attempt, err := s.auth.inspect(r.Context(), account.ID, attemptID, provider)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, errAuthNotFound) {
			status = http.StatusNotFound
		}
		writeError(w, status, "无法读取登录尝试", nil)
		return
	}
	writeJSON(w, http.StatusOK, s.auth.publicAttempt(attempt))
}

func (s *Server) handleCompleteAccountLogin(w http.ResponseWriter, r *http.Request) {
	setAuthResponseHeaders(w)
	s.accountOperationMu.Lock()
	defer s.accountOperationMu.Unlock()
	if err := validateAuthMutation(r); err != nil {
		writeError(w, http.StatusForbidden, "完成登录请求被拒绝", err)
		return
	}
	account, ok := s.accountFromRequest(w, r)
	if !ok {
		return
	}
	provider, err := providerForAccount(account)
	if err != nil {
		writeError(w, http.StatusBadRequest, "账号不支持网页登录", err)
		return
	}
	attemptID := strings.TrimSpace(r.PathValue("attempt"))
	if s.auth == nil || !validAuthAttemptID(attemptID) {
		writeError(w, http.StatusNotFound, "登录尝试不存在", nil)
		return
	}
	var input struct {
		AuthorizationResponse string `json:"authorization_response"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "授权结果格式错误", nil)
		return
	}
	if strings.TrimSpace(input.AuthorizationResponse) == "" || len(input.AuthorizationResponse) > authResponseLimit || strings.IndexByte(input.AuthorizationResponse, 0) >= 0 {
		writeError(w, http.StatusBadRequest, "授权结果为空或过长", nil)
		return
	}
	attempt, err := s.auth.complete(r.Context(), account.ID, attemptID, provider, input.AuthorizationResponse)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, errAuthNotFound) {
			status = http.StatusNotFound
		} else if errors.Is(err, errAuthEnded) {
			status = http.StatusConflict
		}
		writeError(w, status, "无法完成网页登录", nil)
		return
	}
	writeJSON(w, http.StatusOK, s.auth.publicAttempt(attempt))
}

func (s *Server) handleCancelAccountLogin(w http.ResponseWriter, r *http.Request) {
	setAuthResponseHeaders(w)
	s.accountOperationMu.Lock()
	defer s.accountOperationMu.Unlock()
	if err := validateAuthMutation(r); err != nil {
		writeError(w, http.StatusForbidden, "取消登录请求被拒绝", err)
		return
	}
	account, ok := s.accountFromRequest(w, r)
	if !ok {
		return
	}
	attemptID := strings.TrimSpace(r.PathValue("attempt"))
	if s.auth == nil || !validAuthAttemptID(attemptID) {
		writeError(w, http.StatusNotFound, "登录尝试不存在", nil)
		return
	}
	provider, err := providerForAccount(account)
	if err != nil {
		writeError(w, http.StatusBadRequest, "账号不支持网页登录", err)
		return
	}
	if err := s.auth.cancel(r.Context(), account.ID, attemptID, provider); err != nil {
		if errors.Is(err, errAuthCompleted) {
			writeError(w, http.StatusConflict, "登录已经完成", nil)
			return
		}
		if errors.Is(err, errAuthNotFound) {
			writeError(w, http.StatusNotFound, "登录尝试不存在", nil)
			return
		}
		writeError(w, http.StatusBadGateway, "无法取消登录", nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAccountLogout(w http.ResponseWriter, r *http.Request) {
	setAuthResponseHeaders(w)
	s.accountOperationMu.Lock()
	defer s.accountOperationMu.Unlock()
	if err := validateAuthMutation(r); err != nil {
		writeError(w, http.StatusForbidden, "退出登录请求被拒绝", err)
		return
	}
	account, ok := s.accountFromRequest(w, r)
	if !ok {
		return
	}
	provider, err := providerForAccount(account)
	if err != nil {
		writeError(w, http.StatusBadRequest, "账号不支持退出登录", err)
		return
	}
	if count := s.accountReferenceCount(account.ID); count > 0 {
		writeError(w, http.StatusConflict, fmt.Sprintf("仍有 %d 个 Session 使用该账号，请先切换账号", count), nil)
		return
	}
	if s.auth == nil {
		writeError(w, http.StatusServiceUnavailable, "登录助手尚未启用", nil)
		return
	}
	_ = s.auth.cancelCurrent(r.Context(), account.ID)
	result, err := s.auth.runtime.LogoutAuth(r.Context(), model.AuthLogoutRequest{AccountID: account.ID, Provider: provider})
	if err != nil {
		writeError(w, http.StatusBadGateway, "无法清理本地登录凭据", nil)
		return
	}
	if !result.LocalCredentialsRemoved {
		writeError(w, http.StatusBadGateway, "本地登录凭据未能完全清理", nil)
		return
	}
	providerStatus := "unconfirmed"
	if result.ProviderLogoutConfirmed {
		providerStatus = "completed"
	}
	writeJSON(w, http.StatusOK, map[string]any{"state": model.AuthStatusUnauthenticated, "local_credentials_removed": result.LocalCredentialsRemoved, "provider_logout": providerStatus, "message": result.Message})
}

var (
	errAuthCapacity  = errors.New("同时最多运行 3 个登录助手，请稍后重试")
	errAuthCompleted = errors.New("登录已经完成")
	errAuthNotFound  = errors.New("登录尝试不存在")
	errAuthEnded     = errors.New("登录尝试已经结束")
)

func validAuthAttemptID(value string) bool {
	if len(value) != 24 {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}

func randomAuthAttemptID() (string, error) {
	buffer := make([]byte, 15)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buffer)), nil
}

func (m *accountAuthManager) start(ctx context.Context, accountID string, provider model.AuthProvider) (model.AuthRuntimeAttempt, bool, error) {
	m.mu.Lock()
	existingID := m.attemptByAccount[accountID]
	existingState := model.AuthAttemptState("")
	if existing := m.attempts[existingID]; existing != nil {
		existingState = existing.view.State
	}
	m.mu.Unlock()
	if existingID != "" && (!existingState.Terminal() || existingState == model.AuthAttemptExpired) {
		existing, err := m.inspect(ctx, accountID, existingID, provider)
		if err != nil {
			return model.AuthRuntimeAttempt{}, false, err
		}
		if !existing.State.Terminal() {
			return existing, true, nil
		}
	}

	m.mu.Lock()
	expired := m.pruneLocked(time.Now().UTC())
	if attemptID := m.attemptByAccount[accountID]; attemptID != "" {
		if attempt := m.attempts[attemptID]; attempt != nil && !attempt.view.State.Terminal() {
			view := attempt.view
			m.mu.Unlock()
			m.cancelExpired(expired)
			return view, true, nil
		}
	}
	active := 0
	for _, attempt := range m.attempts {
		if !attempt.view.State.Terminal() {
			active++
		}
	}
	if active >= m.maxConcurrent {
		m.mu.Unlock()
		m.cancelExpired(expired)
		return model.AuthRuntimeAttempt{}, false, errAuthCapacity
	}
	attemptID, err := randomAuthAttemptID()
	if err != nil {
		m.mu.Unlock()
		m.cancelExpired(expired)
		return model.AuthRuntimeAttempt{}, false, err
	}
	now := time.Now().UTC()
	seed := model.AuthRuntimeAttempt{AuthAttemptRef: model.AuthAttemptRef{AccountID: accountID, AttemptID: attemptID, Provider: provider}, State: model.AuthAttemptStarting, ExpiresAt: now.Add(m.ttl), PollAfter: 2 * time.Second, Message: "正在准备官方授权"}
	entry := &accountAuthAttempt{view: seed, updatedAt: now}
	m.attempts[attemptID] = entry
	m.attemptByAccount[accountID] = attemptID
	m.mu.Unlock()
	m.cancelExpired(expired)

	entry.runtimeMu.Lock()
	started, startErr := m.runtime.StartAuth(ctx, model.AuthStartRequest{AuthAttemptRef: seed.AuthAttemptRef, ExpiresAt: seed.ExpiresAt})
	entry.runtimeMu.Unlock()
	m.mu.Lock()
	entry = m.attempts[attemptID]
	if entry == nil {
		m.mu.Unlock()
		if startErr == nil {
			m.cancelExpired([]model.AuthAttemptRef{seed.AuthAttemptRef})
		}
		return model.AuthRuntimeAttempt{}, false, errors.New("登录尝试已取消")
	}
	if entry.view.State == model.AuthAttemptCancelling || entry.view.State.Terminal() {
		view := entry.view
		m.mu.Unlock()
		if startErr == nil {
			m.cancelExpired([]model.AuthAttemptRef{seed.AuthAttemptRef})
		}
		return view, false, errors.New("登录尝试已取消")
	}
	if startErr != nil {
		entry.view.State = model.AuthAttemptFailed
		entry.view.ErrorCode = "authorization_start_failed"
		entry.view.Message = "无法准备官方授权"
		entry.view.VerificationURL = ""
		entry.view.UserCode = ""
		entry.view.TerminalAvailable = false
		entry.updatedAt = time.Now().UTC()
		view := entry.view
		m.mu.Unlock()
		return view, false, errors.New("无法准备官方授权")
	}
	entry.view = started
	entry.updatedAt = time.Now().UTC()
	view := entry.view
	m.mu.Unlock()
	return view, false, nil
}

func (m *accountAuthManager) current(accountID string) *model.AuthRuntimeAttempt {
	m.mu.Lock()
	attempt := m.attempts[m.attemptByAccount[accountID]]
	if attempt == nil {
		m.mu.Unlock()
		return nil
	}
	view := attempt.view
	m.mu.Unlock()
	if !view.State.Terminal() && !time.Now().UTC().Before(view.ExpiresAt) {
		view.State = model.AuthAttemptExpired
		view.Message = "登录已过期，请重新开始"
		view.VerificationURL = ""
		view.UserCode = ""
		view.TerminalAvailable = false
	}
	return &view
}

func (m *accountAuthManager) attemptEntry(ctx context.Context, accountID, attemptID string, provider model.AuthProvider) (*accountAuthAttempt, bool, error) {
	m.mu.Lock()
	entry := m.attempts[attemptID]
	if entry != nil {
		if entry.view.AccountID != accountID || entry.view.Provider != provider {
			m.mu.Unlock()
			return nil, false, errAuthNotFound
		}
		m.mu.Unlock()
		return entry, false, nil
	}
	m.mu.Unlock()

	ref := model.AuthAttemptRef{AccountID: accountID, AttemptID: attemptID, Provider: provider}
	fresh, err := m.runtime.InspectAuth(ctx, ref)
	if err != nil {
		return nil, false, errAuthNotFound
	}
	recovered := &accountAuthAttempt{view: fresh, updatedAt: time.Now().UTC()}
	m.mu.Lock()
	defer m.mu.Unlock()
	if entry = m.attempts[attemptID]; entry != nil {
		if entry.view.AccountID != accountID || entry.view.Provider != provider {
			return nil, false, errAuthNotFound
		}
		return entry, false, nil
	}
	m.attempts[attemptID] = recovered
	currentID := m.attemptByAccount[accountID]
	if currentID == "" || m.attempts[currentID] == nil || m.attempts[currentID].view.State.Terminal() {
		m.attemptByAccount[accountID] = attemptID
	}
	return recovered, true, nil
}

func (m *accountAuthManager) inspect(ctx context.Context, accountID, attemptID string, provider model.AuthProvider) (model.AuthRuntimeAttempt, error) {
	entry, recovered, err := m.attemptEntry(ctx, accountID, attemptID, provider)
	if err != nil {
		return model.AuthRuntimeAttempt{}, err
	}
	if recovered {
		return entry.view, nil
	}
	entry.runtimeMu.Lock()
	defer entry.runtimeMu.Unlock()
	m.mu.Lock()
	entry = m.attempts[attemptID]
	if entry == nil || entry.view.AccountID != accountID || entry.view.Provider != provider {
		m.mu.Unlock()
		return model.AuthRuntimeAttempt{}, errAuthNotFound
	}
	cached := entry.view
	m.mu.Unlock()
	if cached.State == model.AuthAttemptCompleted || cached.State == model.AuthAttemptCancelled {
		return cached, nil
	}
	fresh, err := m.runtime.InspectAuth(ctx, cached.AuthAttemptRef)
	if err != nil {
		return model.AuthRuntimeAttempt{}, fmt.Errorf("inspect authorization state: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	entry = m.attempts[attemptID]
	if entry == nil {
		return model.AuthRuntimeAttempt{}, errAuthNotFound
	}
	entry.view = fresh
	entry.updatedAt = time.Now().UTC()
	return entry.view, nil
}

func (m *accountAuthManager) complete(ctx context.Context, accountID, attemptID string, provider model.AuthProvider, response string) (model.AuthRuntimeAttempt, error) {
	operationCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), authCompletionTTL)
	defer cancel()
	ctx = operationCtx
	entry, _, err := m.attemptEntry(ctx, accountID, attemptID, provider)
	if err != nil {
		return model.AuthRuntimeAttempt{}, err
	}
	entry.runtimeMu.Lock()
	defer entry.runtimeMu.Unlock()
	m.mu.Lock()
	entry = m.attempts[attemptID]
	if entry == nil {
		m.mu.Unlock()
		return model.AuthRuntimeAttempt{}, errAuthNotFound
	}
	cached := entry.view
	m.mu.Unlock()
	if cached.State == model.AuthAttemptCompleted {
		return cached, nil
	}
	if cached.State.Terminal() {
		return model.AuthRuntimeAttempt{}, errAuthEnded
	}
	fresh, completeErr := m.runtime.CompleteAuth(ctx, model.AuthCompleteRequest{AuthAttemptRef: cached.AuthAttemptRef, AuthorizationResponse: response})
	if completeErr != nil {
		if inspected, inspectErr := m.runtime.InspectAuth(ctx, cached.AuthAttemptRef); inspectErr == nil {
			m.mu.Lock()
			if current := m.attempts[attemptID]; current != nil {
				current.view = inspected
				current.updatedAt = time.Now().UTC()
			}
			m.mu.Unlock()
			if inspected.State.Terminal() {
				return inspected, nil
			}
		}
		return model.AuthRuntimeAttempt{}, completeErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	entry = m.attempts[attemptID]
	if entry == nil {
		return model.AuthRuntimeAttempt{}, errAuthNotFound
	}
	entry.view = fresh
	entry.updatedAt = time.Now().UTC()
	return entry.view, nil
}

func (m *accountAuthManager) cancel(ctx context.Context, accountID, attemptID string, provider model.AuthProvider) error {
	entry, _, err := m.attemptEntry(ctx, accountID, attemptID, provider)
	if err != nil {
		return err
	}
	entry.runtimeMu.Lock()
	defer entry.runtimeMu.Unlock()
	m.mu.Lock()
	entry = m.attempts[attemptID]
	if entry == nil {
		m.mu.Unlock()
		return errAuthNotFound
	}
	cached := entry.view
	m.mu.Unlock()
	if cached.State != model.AuthAttemptCompleted && !cached.State.Terminal() {
		if fresh, inspectErr := m.runtime.InspectAuth(ctx, cached.AuthAttemptRef); inspectErr == nil {
			cached = fresh
			m.mu.Lock()
			if current := m.attempts[attemptID]; current != nil {
				current.view = fresh
				current.updatedAt = time.Now().UTC()
			}
			m.mu.Unlock()
		}
	}
	if cached.State == model.AuthAttemptCompleted {
		return errAuthCompleted
	}
	if cached.State.Terminal() {
		return nil
	}
	m.mu.Lock()
	entry.view.State = model.AuthAttemptCancelling
	ref := entry.view.AuthAttemptRef
	m.mu.Unlock()
	err = m.runtime.CancelAuth(ctx, ref)
	if fresh, inspectErr := m.runtime.InspectAuth(ctx, ref); inspectErr == nil {
		m.mu.Lock()
		if current := m.attempts[attemptID]; current != nil {
			current.view = fresh
			current.updatedAt = time.Now().UTC()
		}
		m.mu.Unlock()
		if fresh.State == model.AuthAttemptCompleted {
			return errAuthCompleted
		}
		if err == nil {
			return nil
		}
	}
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if entry = m.attempts[attemptID]; entry != nil {
		entry.view.State = model.AuthAttemptCancelled
		entry.view.Message = "登录已取消"
		entry.view.VerificationURL = ""
		entry.view.UserCode = ""
		entry.view.TerminalAvailable = false
		entry.updatedAt = time.Now().UTC()
	}
	return nil
}

func (m *accountAuthManager) cancelCurrent(ctx context.Context, accountID string) error {
	attempt := m.current(accountID)
	if attempt == nil || attempt.State == model.AuthAttemptCompleted || attempt.State == model.AuthAttemptCancelled || attempt.State == model.AuthAttemptFailed {
		return nil
	}
	return m.cancel(ctx, accountID, attempt.AttemptID, attempt.Provider)
}

func (m *accountAuthManager) publicAttempt(attempt model.AuthRuntimeAttempt) loginAttemptView {
	pollAfter := attempt.PollAfter
	if pollAfter < time.Second {
		pollAfter = 2 * time.Second
	}
	createdAt := attempt.ExpiresAt.Add(-m.ttl)
	return loginAttemptView{ID: attempt.AttemptID, AccountID: attempt.AccountID, Provider: attempt.Provider, Flow: "authorization_code", State: attempt.State, CreatedAt: createdAt, ExpiresAt: attempt.ExpiresAt, PollAfterMS: pollAfter.Milliseconds(), VerificationURL: attempt.VerificationURL, Message: attempt.Message, ErrorCode: attempt.ErrorCode}
}

func (m *accountAuthManager) pruneLocked(now time.Time) []model.AuthAttemptRef {
	expired := make([]model.AuthAttemptRef, 0)
	for id, attempt := range m.attempts {
		if !attempt.view.State.Terminal() && !now.Before(attempt.view.ExpiresAt) {
			expired = append(expired, attempt.view.AuthAttemptRef)
			attempt.view.State = model.AuthAttemptExpired
			attempt.view.Message = "登录已过期，请重新开始"
			attempt.view.VerificationURL = ""
			attempt.view.UserCode = ""
			attempt.view.TerminalAvailable = false
			attempt.updatedAt = now
		}
		if attempt.view.State.Terminal() && now.Sub(attempt.updatedAt) > completedAuthKeepTTL {
			delete(m.attempts, id)
			if m.attemptByAccount[attempt.view.AccountID] == id {
				delete(m.attemptByAccount, attempt.view.AccountID)
			}
		}
	}
	return expired
}

func (m *accountAuthManager) cancelExpired(refs []model.AuthAttemptRef) {
	for _, ref := range refs {
		m.mu.Lock()
		entry := m.attempts[ref.AttemptID]
		m.mu.Unlock()
		if entry != nil {
			entry.runtimeMu.Lock()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = m.runtime.CancelAuth(ctx, ref)
		cancel()
		if entry != nil {
			entry.runtimeMu.Unlock()
		}
	}
}

func (m *accountAuthManager) Close(_ context.Context) {
	// Attempts and PKCE state are persisted in the credential volume. One-shot
	// helpers are bounded, so shutdown must not race a credential commit with a
	// second cancellation helper; the next process reconciles the saved state.
}
