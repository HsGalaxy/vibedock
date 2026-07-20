package app

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"vibedock/internal/model"
)

var providerIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,79}$`)

type directAgentInput struct {
	Provider         string `json:"provider"`
	BaseURL          string `json:"base_url"`
	Model            string `json:"model"`
	Protocol         string `json:"protocol"`
	Thinking         string `json:"thinking"`
	PermissionPolicy string `json:"permission_policy"`
	AutoApprove      bool   `json:"auto_approve"`
	Secret           string `json:"secret"`
	ClearSecret      bool   `json:"clear_secret"`
}

func directAgentInputFromForm(r *http.Request) directAgentInput {
	return directAgentInput{
		Provider:         r.FormValue("direct_provider"),
		BaseURL:          r.FormValue("direct_base_url"),
		Model:            r.FormValue("direct_model"),
		Protocol:         r.FormValue("direct_protocol"),
		Thinking:         r.FormValue("direct_thinking"),
		PermissionPolicy: r.FormValue("direct_permission_policy"),
		AutoApprove:      strings.EqualFold(r.FormValue("direct_auto_approve"), "true") || r.FormValue("direct_auto_approve") == "1" || r.FormValue("direct_auto_approve") == "on",
		Secret:           r.FormValue("direct_secret"),
	}
}

func isDirectAgent(agent string) bool {
	agent = normalizeAgent(agent)
	return agent == "pi" || agent == "opencode"
}

func normalizeDirectConfig(agent string, input directAgentInput, existing *model.DirectAgentConfig) (*model.DirectAgentConfig, error) {
	if !isDirectAgent(agent) {
		return nil, nil
	}
	provider := strings.TrimSpace(input.Provider)
	baseURL := strings.TrimRight(strings.TrimSpace(input.BaseURL), "/")
	modelName := strings.TrimSpace(input.Model)
	protocol := normalizeDirectProtocol(input.Protocol)
	thinking := normalizeThinking(input.Thinking)
	permissionPolicy := normalizePermissionPolicy(input.PermissionPolicy)
	if input.PermissionPolicy != "" && permissionPolicy == "" {
		return nil, errors.New("权限策略不合法")
	}
	if provider == "" {
		return nil, errors.New("请填写 Provider ID；Pi 与 OpenCode 不使用统一账号")
	}
	if !providerIDPattern.MatchString(provider) {
		return nil, errors.New("Provider ID 只能包含字母、数字、点、下划线和短横线，且不能超过 80 个字符")
	}
	if len(modelName) > 200 {
		return nil, errors.New("模型名称不能超过 200 个字符")
	}
	if baseURL == "" {
		return nil, errors.New("请填写 Provider Base URL；Pi 与 OpenCode 当前只支持显式的 Session Provider 配置")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil {
		return nil, errors.New("Base URL 必须是无内嵌凭据的有效 http/https 地址")
	}
	if modelName == "" {
		return nil, errors.New("请填写 Provider 模型名称")
	}
	if input.Protocol != "" && protocol == "" {
		return nil, errors.New("不支持的 API 协议")
	}
	if protocol == "" {
		protocol = "openai-completions"
	}
	if input.Thinking != "" && thinking == "" {
		return nil, errors.New("Pi thinking 级别不合法")
	}
	if agent != "pi" {
		thinking = ""
	}
	config := &model.DirectAgentConfig{
		Provider: provider, BaseURL: baseURL, Model: modelName, Protocol: protocol,
		Thinking: thinking, PermissionPolicy: permissionPolicy, AutoApprove: input.AutoApprove,
	}
	if existing != nil {
		config.HasSecret = existing.HasSecret
		config.SecretHint = existing.SecretHint
	}
	if input.ClearSecret {
		config.HasSecret = false
		config.SecretHint = ""
	}
	if strings.TrimSpace(input.Secret) != "" {
		config.HasSecret = true
		config.SecretHint = secretHint(strings.TrimSpace(input.Secret))
	}
	if !config.HasSecret {
		return nil, errors.New("请填写当前 Session 的 API Key / Token")
	}
	return config, nil
}

func normalizeDirectProtocol(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "openai-completions", "chat-completions", "chat":
		return "openai-completions"
	case "openai-responses", "responses":
		return "openai-responses"
	case "anthropic-messages", "anthropic":
		return "anthropic-messages"
	default:
		return ""
	}
}

func normalizePermissionPolicy(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "ask", "safe":
		return "ask"
	case "auto_approve", "always_allow":
		return "auto_approve"
	default:
		return ""
	}
}

func normalizeThinking(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "":
		return ""
	case "off", "minimal", "low", "medium", "high", "xhigh", "max":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return ""
	}
}

func directConfigsEqual(a, b *model.DirectAgentConfig) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Provider == b.Provider &&
		a.BaseURL == b.BaseURL &&
		a.Model == b.Model &&
		a.Protocol == b.Protocol &&
		a.Thinking == b.Thinking &&
		a.PermissionPolicy == b.PermissionPolicy &&
		a.AutoApprove == b.AutoApprove &&
		a.HasSecret == b.HasSecret &&
		a.SecretHint == b.SecretHint
}

func (s *Server) saveDirectSecret(sessionID string, input directAgentInput) error {
	secret := strings.TrimSpace(input.Secret)
	if input.ClearSecret && secret == "" {
		return s.store.DeleteSessionSecret(sessionID)
	}
	if secret == "" {
		return nil
	}
	ciphertext, err := s.keyring.Encrypt(secret)
	if err != nil {
		return fmt.Errorf("加密 Session API Key: %w", err)
	}
	return s.store.PutSessionSecret(sessionID, ciphertext)
}

func (s *Server) directRuntime(session model.Session) (model.DirectAgentRuntime, error) {
	if !isDirectAgent(session.Agent) || session.DirectConfig == nil {
		return model.DirectAgentRuntime{}, nil
	}
	config := session.DirectConfig
	if strings.TrimSpace(config.Provider) == "" || strings.TrimSpace(config.BaseURL) == "" || strings.TrimSpace(config.Model) == "" || !config.HasSecret {
		return model.DirectAgentRuntime{}, errors.New("Session Provider 配置不完整，请填写 Provider ID、Base URL、模型和 API Key / Token")
	}
	secret := ""
	if ciphertext, ok := s.store.GetSessionSecret(session.ID); ok {
		var err error
		secret, err = s.keyring.Decrypt(ciphertext)
		if err != nil {
			return model.DirectAgentRuntime{}, err
		}
	}
	if config.HasSecret && secret == "" {
		return model.DirectAgentRuntime{}, errors.New("Session API Key 已丢失或无法解密")
	}
	return model.DirectAgentRuntime{
		Provider: config.Provider, BaseURL: config.BaseURL, Model: config.Model,
		Protocol: config.Protocol, Thinking: config.Thinking, PermissionPolicy: config.PermissionPolicy,
		AutoApprove: config.AutoApprove || config.PermissionPolicy == "auto_approve", Secret: secret,
	}, nil
}
