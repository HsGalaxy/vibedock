package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"vibedock/internal/model"
)

const maxAccountEnv = 20

var blockedAccountEnv = map[string]bool{
	"PATH": true, "HOME": true, "SHELL": true, "USER": true,
	"LD_PRELOAD": true, "LD_LIBRARY_PATH": true, "BASH_ENV": true, "ENV": true,
	"CODEX_HOME": true, "CLAUDE_CONFIG_DIR": true,
	"OPENAI_API_KEY": true, "ANTHROPIC_API_KEY": true, "ANTHROPIC_AUTH_TOKEN": true,
	"CLAUDE_CODE_OAUTH_TOKEN": true, "VIBEDOCK_PROVIDER_TOKEN": true,
}

type accountInput struct {
	Name        string            `json:"name"`
	Agent       string            `json:"agent"`
	AuthType    string            `json:"auth_type"`
	BaseURL     string            `json:"base_url"`
	Model       string            `json:"model"`
	Secret      string            `json:"secret"`
	ExtraEnv    map[string]string `json:"extra_env"`
	ClearSecret bool              `json:"clear_secret"`
}

func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	agent := normalizeAgentFilter(r.URL.Query().Get("agent"))
	accounts := s.store.ListAccounts()
	views := make([]model.AccountView, 0, len(accounts))
	for _, account := range accounts {
		if agent != "" && account.Agent != agent {
			continue
		}
		views = append(views, accountView(account))
	}
	writeJSON(w, http.StatusOK, views)
}

func (s *Server) handleCreateAccount(w http.ResponseWriter, r *http.Request) {
	var input accountInput
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误", err)
		return
	}
	account, err := s.buildAccount(model.AccountProfile{}, input, true)
	if err != nil {
		writeError(w, http.StatusBadRequest, "账号配置无效", err)
		return
	}
	if err := s.store.PutAccount(account); err != nil {
		writeError(w, http.StatusInternalServerError, "保存账号失败", err)
		return
	}
	writeJSON(w, http.StatusCreated, accountView(account))
}

func (s *Server) handleUpdateAccount(w http.ResponseWriter, r *http.Request) {
	s.accountOperationMu.Lock()
	defer s.accountOperationMu.Unlock()
	account, ok := s.accountFromRequest(w, r)
	if !ok {
		return
	}
	var input accountInput
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误", err)
		return
	}
	updated, err := s.buildAccount(account, input, false)
	if err != nil {
		writeError(w, http.StatusBadRequest, "账号配置无效", err)
		return
	}
	if account.Agent != updated.Agent && s.accountReferenceCount(account.ID) > 0 {
		writeError(w, http.StatusConflict, "该账号已被 Session 使用，不能切换 Agent 类型", nil)
		return
	}
	cleanupNeeded := account.AuthType == "login" && (account.Agent != updated.Agent || updated.AuthType != "login")
	var cleanup model.AuthCredentialCleanup
	if cleanupNeeded {
		if s.auth == nil {
			writeError(w, http.StatusServiceUnavailable, "登录助手尚未启用，无法安全清理旧凭据", nil)
			return
		}
		if err := s.auth.cancelCurrent(r.Context(), account.ID); err != nil {
			writeError(w, http.StatusBadGateway, "无法取消当前网页登录", nil)
			return
		}
		cleanup, err = s.auth.runtime.PrepareAuthCredentialCleanup(r.Context(), account.ID)
		if err != nil {
			writeError(w, http.StatusBadGateway, "无法暂存旧的官方 CLI 登录凭据", nil)
			return
		}
	}
	if err := s.store.PutAccount(updated); err != nil {
		if cleanupNeeded {
			if rollbackErr := s.auth.runtime.RollbackAuthCredentialCleanup(r.Context(), cleanup); rollbackErr != nil {
				s.logger.Error("rollback account credential cleanup", "account", account.ID, "error", rollbackErr)
			}
		}
		writeError(w, http.StatusInternalServerError, "保存账号失败", err)
		return
	}
	if cleanupNeeded {
		if err := s.auth.runtime.CommitAuthCredentialCleanup(r.Context(), cleanup); err != nil {
			s.logger.Error("commit account credential cleanup", "account", account.ID, "error", err)
			writeError(w, http.StatusInternalServerError, "账号已更新，但旧凭据清理尚待恢复", nil)
			return
		}
	}
	writeJSON(w, http.StatusOK, accountView(updated))
}

func (s *Server) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	s.accountOperationMu.Lock()
	defer s.accountOperationMu.Unlock()
	account, ok := s.accountFromRequest(w, r)
	if !ok {
		return
	}
	if count := s.accountReferenceCount(account.ID); count > 0 {
		writeError(w, http.StatusConflict, fmt.Sprintf("仍有 %d 个 Session 使用该账号，请先切换账号", count), nil)
		return
	}
	var cleanup model.AuthCredentialCleanup
	cleanupNeeded := account.AuthType == "login"
	if cleanupNeeded {
		if s.auth == nil {
			writeError(w, http.StatusServiceUnavailable, "登录助手尚未启用，无法安全清理账号凭据", nil)
			return
		}
		if err := s.auth.cancelCurrent(r.Context(), account.ID); err != nil {
			writeError(w, http.StatusBadGateway, "无法取消当前网页登录", nil)
			return
		}
		var err error
		cleanup, err = s.auth.runtime.PrepareAuthCredentialCleanup(r.Context(), account.ID)
		if err != nil {
			writeError(w, http.StatusBadGateway, "无法暂存账号的本地登录凭据", nil)
			return
		}
	}
	if err := s.store.DeleteAccount(account.ID); err != nil {
		if cleanupNeeded {
			if rollbackErr := s.auth.runtime.RollbackAuthCredentialCleanup(r.Context(), cleanup); rollbackErr != nil {
				s.logger.Error("rollback deleted account credentials", "account", account.ID, "error", rollbackErr)
			}
		}
		writeError(w, http.StatusInternalServerError, "删除账号失败", err)
		return
	}
	if cleanupNeeded {
		if err := s.auth.runtime.CommitAuthCredentialCleanup(r.Context(), cleanup); err != nil {
			s.logger.Error("commit deleted account credential cleanup", "account", account.ID, "error", err)
			writeError(w, http.StatusInternalServerError, "账号已删除，但本地凭据清理尚待恢复", nil)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleProbeAccount(w http.ResponseWriter, r *http.Request) {
	account, ok := s.accountFromRequest(w, r)
	if !ok {
		return
	}
	if account.AuthType == "login" {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "status": "interactive", "message": "请在“账号与 Provider”中使用官方网页登录入口，并通过账号登录状态检查验证。",
		})
		return
	}
	if account.AuthType == "oauth_token" {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "status": "session_required", "message": "Claude setup-token / OAuth Token 不使用 Models API 探测，请启动或重启一个绑定该账号的 Session 验证。",
		})
		return
	}
	secret, err := s.keyring.Decrypt(account.SecretCiphertext)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "无法解密账号凭据", err)
		return
	}
	if secret == "" {
		writeError(w, http.StatusBadRequest, "账号没有可用密钥", nil)
		return
	}
	result, err := probeAccount(r.Context(), account, secret)
	if err != nil {
		writeError(w, http.StatusBadGateway, "连接测试失败", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) buildAccount(existing model.AccountProfile, input accountInput, creating bool) (model.AccountProfile, error) {
	account := existing
	if creating {
		id, err := randomID()
		if err != nil {
			return model.AccountProfile{}, err
		}
		account.ID = id
		account.CreatedAt = time.Now().UTC()
	}

	name := strings.TrimSpace(input.Name)
	if name == "" || len(name) > 80 {
		return model.AccountProfile{}, errors.New("名称不能为空且不能超过 80 个字符")
	}
	agent := normalizeAccountAgent(input.Agent)
	if agent == "" {
		return model.AccountProfile{}, errors.New("Agent 必须是 codex 或 claude")
	}
	authType := normalizeAuthType(input.AuthType)
	if err := validateAuthType(agent, authType); err != nil {
		return model.AccountProfile{}, err
	}

	baseURL := strings.TrimSpace(input.BaseURL)
	if baseURL != "" {
		parsed, err := url.Parse(baseURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil {
			return model.AccountProfile{}, errors.New("Base URL 必须是无内嵌凭据的 http/https 地址")
		}
		baseURL = strings.TrimRight(baseURL, "/")
	}
	modelName := strings.TrimSpace(input.Model)
	if len(modelName) > 160 {
		return model.AccountProfile{}, errors.New("模型名称不能超过 160 个字符")
	}

	extraEnv := input.ExtraEnv
	validatedEnv, err := validateAccountEnv(extraEnv)
	if err != nil {
		return model.AccountProfile{}, err
	}

	ciphertext := existing.SecretCiphertext
	hint := existing.SecretHint
	secret := strings.TrimSpace(input.Secret)
	if input.ClearSecret || authType == "login" {
		ciphertext = ""
		hint = ""
	}
	if secret != "" && authType != "login" {
		ciphertext, err = s.keyring.Encrypt(secret)
		if err != nil {
			return model.AccountProfile{}, err
		}
		hint = secretHint(secret)
	}
	if authType != "login" && ciphertext == "" {
		return model.AccountProfile{}, errors.New("此认证方式需要密钥或 Token")
	}

	account.Name = name
	account.Agent = agent
	account.AuthType = authType
	account.BaseURL = baseURL
	account.Model = modelName
	account.ExtraEnv = validatedEnv
	account.SecretCiphertext = ciphertext
	account.SecretHint = hint
	account.UpdatedAt = time.Now().UTC()
	return account, nil
}

func (s *Server) accountFromRequest(w http.ResponseWriter, r *http.Request) (model.AccountProfile, bool) {
	id := strings.TrimSpace(r.PathValue("account"))
	if !idPattern.MatchString(id) {
		writeError(w, http.StatusBadRequest, "账号 ID 不合法", nil)
		return model.AccountProfile{}, false
	}
	account, ok := s.store.GetAccount(id)
	if !ok {
		writeError(w, http.StatusNotFound, "账号不存在", nil)
		return model.AccountProfile{}, false
	}
	return account, true
}

func (s *Server) validateAccountSelection(agent, accountID string) error {
	if isDirectAgent(agent) {
		if accountID != "" {
			return errors.New("Pi 与 OpenCode 使用 Session 参数，不使用统一账号")
		}
		return nil
	}
	if accountID == "" || normalizeAgent(agent) == "shell" {
		return nil
	}
	if !idPattern.MatchString(accountID) {
		return errors.New("账号 ID 不合法")
	}
	account, ok := s.store.GetAccount(accountID)
	if !ok {
		return errors.New("账号不存在")
	}
	if account.Agent != normalizeAgent(agent) {
		return fmt.Errorf("%s 账号不能用于 %s Session", account.Agent, normalizeAgent(agent))
	}
	return nil
}

func (s *Server) runtimeSession(ws model.Workspace, session model.Session) (model.SessionRuntimeRequest, error) {
	req := model.SessionRuntimeRequest{
		WorkspaceID:              ws.ID,
		SessionID:                session.ID,
		Agent:                    session.Agent,
		Port:                     session.Port,
		Password:                 ws.Password,
		PermissionMode:           session.PermissionMode,
		DangerousSkipPermissions: session.DangerousSkipPermissions,
		Account:                  model.AccountRuntime{AuthType: "workspace"},
	}
	if isDirectAgent(session.Agent) {
		direct, err := s.directRuntime(session)
		if err != nil {
			return model.SessionRuntimeRequest{}, err
		}
		req.Direct = direct
	} else if session.AccountID != "" && session.Agent != "shell" {
		account, ok := s.store.GetAccount(session.AccountID)
		if !ok {
			return model.SessionRuntimeRequest{}, errors.New("Session 绑定的账号已不存在")
		}
		if s.auth != nil {
			if attempt := s.auth.current(account.ID); attempt != nil && !attempt.State.Terminal() {
				return model.SessionRuntimeRequest{}, errors.New("账号正在完成官方登录，请完成或取消登录后再启动 Session")
			}
		}
		if account.Agent != session.Agent {
			return model.SessionRuntimeRequest{}, errors.New("Session 与账号的 Agent 类型不匹配")
		}
		secret, err := s.keyring.Decrypt(account.SecretCiphertext)
		if err != nil {
			return model.SessionRuntimeRequest{}, err
		}
		if account.AuthType != "login" && secret == "" {
			return model.SessionRuntimeRequest{}, errors.New("账号凭据为空")
		}
		req.Account = model.AccountRuntime{
			ID: account.ID, Name: account.Name, Agent: account.Agent, AuthType: account.AuthType,
			BaseURL: account.BaseURL, Model: account.Model, Secret: secret, ExtraEnv: account.ExtraEnv,
		}
		now := time.Now().UTC()
		if account.LastUsedAt == nil || now.Sub(*account.LastUsedAt) > 5*time.Minute {
			account.LastUsedAt = &now
			account.UpdatedAt = now
			_ = s.store.PutAccount(account)
		}
	}
	command, err := compileLaunchCommand(session.Agent, session.LaunchProfile, req.Account, req.Direct, session.ID, session.PermissionMode, session.DangerousSkipPermissions)
	if err != nil {
		return model.SessionRuntimeRequest{}, err
	}
	req.LaunchCommand = command
	if session.LaunchProfile != nil && (session.Agent == "pi" || session.Agent == "opencode") {
		req.ConfigMode = session.LaunchProfile.ConfigMode
		req.CustomConfig = session.LaunchProfile.CustomConfig
	}
	return req, nil
}

func (s *Server) accountReferenceCount(accountID string) int {
	count := 0
	for _, ws := range s.store.List() {
		for _, session := range ws.Sessions {
			if session.AccountID == accountID {
				count++
			}
		}
	}
	return count
}

func (s *Server) activeAccountReferenceCount(accountID string) int {
	count := 0
	for _, ws := range s.store.List() {
		if ws.Status != "running" && ws.Status != "creating" {
			continue
		}
		for _, session := range ws.Sessions {
			if session.AccountID == accountID {
				count++
			}
		}
	}
	return count
}

func accountView(account model.AccountProfile) model.AccountView {
	return model.AccountView{
		ID: account.ID, Name: account.Name, Agent: account.Agent, AuthType: account.AuthType,
		BaseURL: account.BaseURL, Model: account.Model, ExtraEnv: cloneStringMap(account.ExtraEnv),
		HasSecret: account.SecretCiphertext != "", SecretHint: account.SecretHint,
		CreatedAt: account.CreatedAt, UpdatedAt: account.UpdatedAt, LastUsedAt: account.LastUsedAt,
	}
}

func normalizeAccountAgent(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "codex":
		return "codex"
	case "claude", "claude-code":
		return "claude"
	default:
		return ""
	}
}

func normalizeAgentFilter(value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return normalizeAccountAgent(value)
}

func normalizeAuthType(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "login", "native":
		return "login"
	case "api_key", "api-key", "key":
		return "api_key"
	case "bearer_token", "bearer-token", "bearer":
		return "bearer_token"
	case "oauth_token", "oauth-token", "oauth":
		return "oauth_token"
	default:
		return ""
	}
}

func validateAuthType(agent, authType string) error {
	switch agent {
	case "codex":
		if authType == "login" || authType == "api_key" || authType == "bearer_token" {
			return nil
		}
	case "claude":
		if authType == "login" || authType == "api_key" || authType == "bearer_token" || authType == "oauth_token" {
			return nil
		}
	}
	return fmt.Errorf("%s 不支持认证方式 %q", agent, authType)
}

func validateAccountEnv(values map[string]string) (map[string]string, error) {
	if len(values) > maxAccountEnv {
		return nil, fmt.Errorf("附加环境变量最多 %d 个", maxAccountEnv)
	}
	out := make(map[string]string, len(values))
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := values[key]
		key = strings.TrimSpace(key)
		upperKey := strings.ToUpper(key)
		sensitiveName := strings.Contains(upperKey, "TOKEN") || strings.Contains(upperKey, "SECRET") || strings.Contains(upperKey, "PASSWORD") || strings.HasSuffix(upperKey, "_KEY") || strings.Contains(upperKey, "API_KEY")
		if !envKeyPattern.MatchString(key) || blockedAccountEnv[upperKey] || sensitiveName || strings.HasPrefix(upperKey, "VIBEDOCK_") || strings.HasPrefix(upperKey, "WORKSPACE_") || strings.HasPrefix(upperKey, "SESSION_") {
			return nil, fmt.Errorf("不允许的附加环境变量：%s（敏感值请使用主密钥字段）", key)
		}
		if len(value) > 4096 {
			return nil, fmt.Errorf("环境变量 %s 的值过长", key)
		}
		out[key] = value
	}
	return out, nil
}

func secretHint(secret string) string {
	runes := []rune(secret)
	if len(runes) <= 4 {
		return "••••"
	}
	return "••••" + string(runes[len(runes)-4:])
}

func cloneStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func redactURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "[redacted]"
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func validateProbeEndpoint(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil {
		return errors.New("Provider 探测仅支持无内嵌凭据的 HTTPS 地址")
	}
	addresses, err := net.LookupIP(parsed.Hostname())
	if err != nil {
		return fmt.Errorf("解析 Provider 主机失败: %w", err)
	}
	for _, address := range addresses {
		if address.IsLoopback() || address.IsPrivate() || address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() || address.IsUnspecified() {
			return errors.New("Provider 探测不能访问本机或私有网络地址")
		}
	}
	return nil
}

func probeAccount(ctx context.Context, account model.AccountProfile, secret string) (map[string]any, error) {
	baseURL := strings.TrimRight(account.BaseURL, "/")
	if parsed, err := url.Parse(baseURL); baseURL != "" && (err != nil || parsed.User != nil || parsed.Host == "") {
		return nil, errors.New("Provider Base URL 无效或包含内嵌凭据")
	}
	if baseURL == "" {
		if account.Agent == "codex" {
			baseURL = "https://api.openai.com/v1"
		} else {
			baseURL = "https://api.anthropic.com"
		}
	}
	endpoint := baseURL + "/models"
	if account.Agent == "claude" && !strings.HasSuffix(baseURL, "/v1") {
		endpoint = baseURL + "/v1/models"
	}
	if err := validateProbeEndpoint(endpoint); err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	if account.Agent == "claude" && account.AuthType == "api_key" {
		request.Header.Set("x-api-key", secret)
		request.Header.Set("anthropic-version", "2023-06-01")
	} else {
		request.Header.Set("Authorization", "Bearer "+secret)
	}
	client := &http.Client{
		Timeout: 12 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	status := "reachable"
	ok := response.StatusCode >= 200 && response.StatusCode < 300
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		status = "auth_failed"
	} else if ok {
		status = "ready"
	}
	return map[string]any{
		"ok":          ok,
		"status":      status,
		"http_status": response.StatusCode,
		"endpoint":    redactURL(endpoint),
	}, nil
}
