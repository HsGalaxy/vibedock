package app

import (
	"errors"
	"net/http"
	"strings"

	"vibedock/internal/model"
)

func accountConfigProvider(account model.AccountProfile) (model.AuthProvider, error) {
	if account.Agent == "codex" {
		return model.AuthProviderCodex, nil
	}
	if account.Agent == "claude" {
		return model.AuthProviderClaude, nil
	}
	return "", errors.New("当前账号不支持配置文件编辑")
}

func (s *Server) accountConfigRequest(w http.ResponseWriter, r *http.Request) (model.AccountProfile, model.AuthProvider, bool) {
	if s.configRuntime == nil {
		writeError(w, http.StatusNotImplemented, "当前运行时不支持账号配置文件编辑", nil)
		return model.AccountProfile{}, "", false
	}
	account, ok := s.accountFromRequest(w, r)
	if !ok {
		return model.AccountProfile{}, "", false
	}
	provider, err := accountConfigProvider(account)
	if err != nil {
		writeError(w, http.StatusBadRequest, "账号配置文件不可用", err)
		return model.AccountProfile{}, "", false
	}
	return account, provider, true
}

func (s *Server) handleAccountConfigList(w http.ResponseWriter, r *http.Request) {
	account, provider, ok := s.accountConfigRequest(w, r)
	if !ok {
		return
	}
	files, err := s.configRuntime.ListAccountConfig(r.Context(), account.ID, provider)
	if err != nil {
		writeError(w, http.StatusBadGateway, "无法读取账号配置文件", err)
		return
	}
	setConfigResponseHeaders(w)
	writeJSON(w, http.StatusOK, map[string]any{"agent": account.Agent, "files": files})
}

func (s *Server) handleAccountConfigFile(w http.ResponseWriter, r *http.Request) {
	account, provider, ok := s.accountConfigRequest(w, r)
	if !ok {
		return
	}
	path := strings.TrimSpace(r.PathValue("path"))
	if r.Method == http.MethodGet {
		document, err := s.configRuntime.ReadAccountConfig(r.Context(), account.ID, provider, path)
		if err != nil {
			writeError(w, http.StatusBadRequest, "无法读取账号配置文件", err)
			return
		}
		setConfigResponseHeaders(w)
		writeJSON(w, http.StatusOK, document)
		return
	}
	if r.Method != http.MethodPut && r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "不支持的配置文件操作", nil)
		return
	}
	if err := validateAuthMutation(r); err != nil {
		writeError(w, http.StatusForbidden, "拒绝账号配置文件修改", err)
		return
	}
	s.accountOperationMu.Lock()
	defer s.accountOperationMu.Unlock()
	if r.Method == http.MethodDelete {
		if err := s.configRuntime.ResetAccountConfig(r.Context(), account.ID, provider, path); err != nil {
			writeError(w, http.StatusBadRequest, "无法重置账号配置文件", err)
			return
		}
		setConfigResponseHeaders(w)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var input struct {
		Content string `json:"content"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "配置文件请求格式错误", err)
		return
	}
	document, err := s.configRuntime.WriteAccountConfig(r.Context(), account.ID, provider, path, input.Content)
	if err != nil {
		writeError(w, http.StatusBadRequest, "无法保存账号配置文件", err)
		return
	}
	setConfigResponseHeaders(w)
	writeJSON(w, http.StatusOK, document)
}

func setConfigResponseHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store, private")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}
