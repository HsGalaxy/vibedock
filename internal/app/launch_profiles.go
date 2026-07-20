package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"unicode"

	"vibedock/internal/model"
)

const (
	maxLaunchArgs       = 64
	maxLaunchTokenBytes = 256
	maxLaunchConfigSize = 256 << 10
)

func normalizeLaunchProfile(agent string, input *model.AgentLaunchProfile) (*model.AgentLaunchProfile, error) {
	if input == nil {
		return nil, nil
	}
	agent = normalizeAgent(agent)
	profile := *input
	profile.Version = 1
	profile.Model = strings.TrimSpace(profile.Model)
	if len(profile.Model) > 200 {
		return nil, errors.New("启动模型名称不能超过 200 个字符")
	}
	if agent != "codex" {
		profile.CodexApproval, profile.CodexSandbox = "", ""
		profile.CodexWebSearch = false
		profile.CodexConfigOverrides = nil
	}
	if agent != "claude" {
		profile.ClaudeContinue = false
	}
	if agent != "opencode" {
		profile.OpenCodeContinue = false
	}
	if profile.CodexApproval != "" {
		switch profile.CodexApproval {
		case "untrusted", "on-request", "never":
		default:
			return nil, errors.New("Codex approval policy 不受支持")
		}
	}
	if profile.CodexSandbox != "" {
		switch profile.CodexSandbox {
		case "read-only", "workspace-write", "danger-full-access":
		default:
			return nil, errors.New("Codex sandbox mode 不受支持")
		}
	}
	if len(profile.CodexConfigOverrides) > 32 {
		return nil, errors.New("Codex 配置覆盖项过多")
	}
	for index, value := range profile.CodexConfigOverrides {
		value = strings.TrimSpace(value)
		if value == "" || len(value) > maxLaunchTokenBytes || hasUnsafeLaunchText(value) {
			return nil, fmt.Errorf("Codex 配置覆盖项 %d 无效", index+1)
		}
		profile.CodexConfigOverrides[index] = value
	}
	if len(profile.ExtraArgs) > maxLaunchArgs {
		return nil, errors.New("高级启动参数不能超过 64 项")
	}
	for index, value := range profile.ExtraArgs {
		if value == "" || len(value) > maxLaunchTokenBytes || hasUnsafeLaunchText(value) {
			return nil, fmt.Errorf("高级启动参数第 %d 项无效", index+1)
		}
		profile.ExtraArgs[index] = value
	}
	if profile.ConfigMode == "" {
		profile.ConfigMode = "managed"
	}
	if agent != "pi" && agent != "opencode" {
		profile.ConfigMode, profile.CustomConfig = "", ""
	} else {
		switch profile.ConfigMode {
		case "managed":
			profile.CustomConfig = ""
		case "custom":
			if err := validateCustomAgentConfig(agent, profile.CustomConfig); err != nil {
				return nil, err
			}
		default:
			return nil, errors.New("配置模式必须是 managed 或 custom")
		}
	}
	return &profile, nil
}

func launchProfilesEqual(left, right *model.AgentLaunchProfile) bool {
	return reflect.DeepEqual(left, right)
}

func hasUnsafeLaunchText(value string) bool {
	for _, runeValue := range value {
		if runeValue == '\x00' || unicode.IsControl(runeValue) {
			return true
		}
	}
	return false
}

func validateCustomAgentConfig(agent, content string) error {
	if len(content) == 0 || len(content) > maxLaunchConfigSize {
		return errors.New("自定义配置不能为空且不能超过 256 KiB")
	}
	for _, runeValue := range content {
		if runeValue == '\x00' || (unicode.IsControl(runeValue) && runeValue != '\n' && runeValue != '\r' && runeValue != '\t') {
			return errors.New("自定义配置包含不可用控制字符")
		}
	}
	var document any
	if err := json.Unmarshal([]byte(content), &document); err != nil {
		return fmt.Errorf("自定义配置必须是有效 JSON: %w", err)
	}
	if _, ok := document.(map[string]any); !ok {
		return errors.New("自定义配置根节点必须是 JSON 对象")
	}
	required := "$VIBEDOCK_DIRECT_API_KEY"
	if agent == "opencode" {
		required = "{env:VIBEDOCK_DIRECT_API_KEY}"
	}
	foundPlaceholder := false
	var walk func(any) error
	walk = func(value any) error {
		switch typed := value.(type) {
		case map[string]any:
			for key, child := range typed {
				normalizedKey := strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(key))
				if normalizedKey == "apikey" || normalizedKey == "token" || normalizedKey == "secret" || normalizedKey == "password" {
					text, ok := child.(string)
					if !ok || text != required {
						return fmt.Errorf("自定义配置中的 %s 必须使用运行时密钥占位符", key)
					}
					foundPlaceholder = true
				}
				if err := walk(child); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range typed {
				if err := walk(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(document); err != nil {
		return err
	}
	if !foundPlaceholder {
		return fmt.Errorf("自定义配置必须通过 %s 引用 Session 密钥", required)
	}
	return nil
}

func managedAgentConfig(agent string, direct model.DirectAgentRuntime) string {
	if agent == "pi" {
		value := map[string]any{"providers": map[string]any{direct.Provider: map[string]any{
			"apiKey": "$VIBEDOCK_DIRECT_API_KEY", "baseUrl": strings.TrimRight(direct.BaseURL, "/"),
			"api": direct.Protocol, "models": []any{map[string]any{"id": direct.Model, "name": direct.Model}},
		}}}
		encoded, _ := json.MarshalIndent(value, "", "  ")
		return string(encoded) + "\n"
	}
	provider := map[string]any{
		"name": "VibeDock session provider", "npm": openCodeNPM(direct.Protocol),
		"options": map[string]any{"apiKey": "{env:VIBEDOCK_DIRECT_API_KEY}", "baseURL": strings.TrimRight(direct.BaseURL, "/")},
		"models":  map[string]any{direct.Model: map[string]any{"name": direct.Model}},
	}
	value := map[string]any{
		"$schema": "https://opencode.ai/config.json", "provider": map[string]any{direct.Provider: provider},
		"model": direct.Provider + "/" + direct.Model,
	}
	encoded, _ := json.MarshalIndent(value, "", "  ")
	return string(encoded) + "\n"
}

func openCodeNPM(protocol string) string {
	switch protocol {
	case "anthropic-messages":
		return "@ai-sdk/anthropic"
	case "openai-responses":
		return "@ai-sdk/openai"
	default:
		return "@ai-sdk/openai-compatible"
	}
}

func compileLaunchCommand(agent string, profile *model.AgentLaunchProfile, account model.AccountRuntime, direct model.DirectAgentRuntime, sessionID, permissionMode string, dangerousSkip bool) (model.LaunchCommand, error) {
	agent = normalizeAgent(agent)
	if profile == nil {
		profile = &model.AgentLaunchProfile{}
	}
	command := model.LaunchCommand{}
	modelName := strings.TrimSpace(profile.Model)
	if modelName == "" {
		if agent == "pi" || agent == "opencode" {
			modelName = direct.Model
		} else {
			modelName = account.Model
		}
	}
	switch agent {
	case "codex":
		command.Executable = "codex"
		managedProviderModel := profile.Model == "" && (account.AuthType == "api_key" || account.AuthType == "bearer_token")
		if account.AuthType == "api_key" || account.AuthType == "bearer_token" {
			command.Arguments = append(command.Arguments, "--profile", "vibedock")
		}
		if modelName != "" && !managedProviderModel {
			command.Arguments = append(command.Arguments, "--model", modelName)
		}
		if profile.CodexSandbox != "" {
			command.Arguments = append(command.Arguments, "--sandbox", profile.CodexSandbox)
		}
		if profile.CodexApproval != "" {
			command.Arguments = append(command.Arguments, "--ask-for-approval", profile.CodexApproval)
		}
		if profile.CodexWebSearch {
			command.Arguments = append(command.Arguments, "--search")
		}
		for _, override := range profile.CodexConfigOverrides {
			command.Arguments = append(command.Arguments, "-c", override)
		}
	case "claude":
		command.Executable = "claude"
		if modelName != "" {
			command.Arguments = append(command.Arguments, "--model", modelName)
		}
		if profile.ClaudeContinue {
			command.Arguments = append(command.Arguments, "--continue")
		}
		if permissionMode != "" && permissionMode != "manual" {
			command.Arguments = append(command.Arguments, "--permission-mode", permissionMode)
		}
		if dangerousSkip {
			command.Arguments = append(command.Arguments, "--allow-dangerously-skip-permissions", "--dangerously-skip-permissions")
		}
	case "pi":
		command.Executable = "pi"
		command.Arguments = append(command.Arguments, "--session-dir", "$HOME/.vibedock/agents/"+sessionID+"/pi/sessions")
		if direct.Provider != "" {
			command.Arguments = append(command.Arguments, "--provider", direct.Provider)
		}
		if modelName != "" {
			command.Arguments = append(command.Arguments, "--model", modelName)
		}
		if direct.Thinking != "" {
			command.Arguments = append(command.Arguments, "--thinking", direct.Thinking)
		}
		if direct.AutoApprove || direct.PermissionPolicy == "auto_approve" {
			command.Arguments = append(command.Arguments, "--approve")
		}
	case "opencode":
		command.Executable = "opencode"
		if modelName != "" {
			command.Arguments = append(command.Arguments, "--model", direct.Provider+"/"+modelName)
		}
		if direct.AutoApprove || direct.PermissionPolicy == "auto_approve" {
			command.Arguments = append(command.Arguments, "--auto")
		}
		if profile.OpenCodeContinue {
			command.Arguments = append(command.Arguments, "--continue")
		}
	default:
		command.Executable = "/bin/bash"
		command.Arguments = []string{"-l"}
	}
	command.Arguments = append(command.Arguments, profile.ExtraArgs...)
	if agent == "opencode" {
		command.Arguments = append(command.Arguments, "/workspace")
	}
	command.Preview = shellJoin(command.Executable, command.Arguments)
	return command, nil
}

func shellJoin(executable string, arguments []string) string {
	parts := []string{previewShellQuote(executable)}
	for _, argument := range arguments {
		parts = append(parts, previewShellQuote(argument))
	}
	return strings.Join(parts, " ")
}

func previewShellQuote(value string) string {
	if value != "" && !strings.ContainsAny(value, " \t\r\n'\"\\$`;&|<>*?()[]{}!") {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func launchCommandJSON(command model.LaunchCommand) string {
	encoded, _ := json.Marshal(struct {
		Executable string   `json:"executable"`
		Arguments  []string `json:"arguments"`
	}{command.Executable, command.Arguments})
	return string(encoded)
}

func (s *Server) handleLaunchPreview(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Agent                    string                    `json:"agent"`
		AccountID                string                    `json:"account_id"`
		SessionID                string                    `json:"session_id"`
		PermissionMode           string                    `json:"permission_mode"`
		DangerousSkipPermissions bool                      `json:"dangerous_skip_permissions"`
		DirectConfig             directAgentInput          `json:"direct_config"`
		LaunchProfile            *model.AgentLaunchProfile `json:"launch_profile"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误", err)
		return
	}
	agent := normalizeAgent(input.Agent)
	profile, err := normalizeLaunchProfile(agent, input.LaunchProfile)
	if err != nil {
		writeError(w, http.StatusBadRequest, "启动参数无效", err)
		return
	}
	permissionMode := strings.TrimSpace(input.PermissionMode)
	if permissionMode == "" {
		permissionMode = "manual"
	}
	if agent == "claude" {
		switch permissionMode {
		case "manual", "acceptEdits", "auto", "dontAsk", "plan", "bypassPermissions":
		default:
			writeError(w, http.StatusBadRequest, "权限配置无效", errors.New("Claude 权限模式不受支持"))
			return
		}
	}
	account := model.AccountRuntime{AuthType: "workspace"}
	if accountID := strings.TrimSpace(input.AccountID); accountID != "" {
		if !idPattern.MatchString(accountID) {
			writeError(w, http.StatusBadRequest, "账号 ID 不合法", nil)
			return
		}
		stored, ok := s.store.GetAccount(accountID)
		if !ok || stored.Agent != agent {
			writeError(w, http.StatusBadRequest, "账号选择无效", nil)
			return
		}
		account = model.AccountRuntime{ID: stored.ID, Agent: stored.Agent, AuthType: stored.AuthType, Model: stored.Model}
	}
	direct := model.DirectAgentRuntime{
		Provider: strings.TrimSpace(input.DirectConfig.Provider), Model: strings.TrimSpace(input.DirectConfig.Model),
		BaseURL: strings.TrimSpace(input.DirectConfig.BaseURL), Protocol: normalizeDirectProtocol(input.DirectConfig.Protocol),
		Thinking: normalizeThinking(input.DirectConfig.Thinking), PermissionPolicy: normalizePermissionPolicy(input.DirectConfig.PermissionPolicy),
		AutoApprove: input.DirectConfig.AutoApprove,
	}
	sessionID := strings.TrimSpace(input.SessionID)
	if sessionID == "" {
		sessionID = "<session-id>"
	} else if !idPattern.MatchString(sessionID) {
		writeError(w, http.StatusBadRequest, "Session ID 不合法", nil)
		return
	}
	command, err := compileLaunchCommand(agent, profile, account, direct, sessionID, permissionMode, input.DangerousSkipPermissions)
	if err != nil {
		writeError(w, http.StatusBadRequest, "无法生成启动命令", err)
		return
	}
	response := map[string]any{"command": command}
	if agent == "pi" || agent == "opencode" {
		path := "models.json"
		if agent == "opencode" {
			path = "opencode.json"
		}
		response["config"] = map[string]any{
			"path": path, "mode": profileConfigMode(profile), "managed_template": managedAgentConfig(agent, direct),
		}
	}
	w.Header().Set("Cache-Control", "no-store, private")
	writeJSON(w, http.StatusOK, response)
}

func profileConfigMode(profile *model.AgentLaunchProfile) string {
	if profile != nil && profile.ConfigMode != "" {
		return profile.ConfigMode
	}
	return "managed"
}
