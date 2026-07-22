package app

import (
	"archive/zip"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"vibedock/internal/model"
	"vibedock/internal/secrets"
	"vibedock/internal/store"
)

const (
	maxUploadBytes    = 512 << 20
	maxExtractBytes   = int64(2 << 30)
	maxZipFiles       = 20000
	firstSessionPort  = 7700
	lastSessionPort   = 7999
	codeServerPort    = 8080
	maxTranscriptRows = 5000
)

var (
	idPattern      = regexp.MustCompile(`^[a-z0-9]{12}$`)
	envKeyPattern  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	nameSanitizer  = regexp.MustCompile(`[^a-zA-Z0-9-]+`)
	dockerNameTrim = regexp.MustCompile(`^-+|-+$`)
)

type Server struct {
	store                   *store.Store
	runtime                 model.Runtime
	templates               map[string]model.Template
	templateList            []model.Template
	web                     fs.FS
	logger                  *slog.Logger
	keyring                 *secrets.Keyring
	ensureMu                sync.Mutex
	ensureCalls             map[string]*ensureSessionCall
	accountOperationMu      sync.Mutex
	auth                    *accountAuthManager
	configRuntime           model.AccountConfigRuntime
	controlPlaneAuthEnabled bool
}

type ensureSessionCall struct {
	done chan struct{}
	err  error
}

func NewServer(st *store.Store, runtime model.Runtime, templates []model.Template, web fs.FS, logger *slog.Logger, keyring *secrets.Keyring) *Server {
	return NewServerWithAuth(st, runtime, nil, templates, web, logger, keyring, AccountAuthOptions{})
}

func NewServerWithAuth(st *store.Store, runtime model.Runtime, authRuntime model.AuthRuntime, templates []model.Template, web fs.FS, logger *slog.Logger, keyring *secrets.Keyring, authOptions AccountAuthOptions) *Server {
	byID := make(map[string]model.Template, len(templates))
	for _, item := range templates {
		byID[item.ID] = item
	}
	return &Server{
		store: st, runtime: runtime, templates: byID,
		templateList: append([]model.Template(nil), templates...), web: web,
		logger: logger, keyring: keyring, ensureCalls: make(map[string]*ensureSessionCall),
		auth:                    newAccountAuthManager(authRuntime, authOptions),
		configRuntime:           accountConfigRuntime(authRuntime),
		controlPlaneAuthEnabled: authOptions.ControlPlaneAuthEnabled,
	}
}

func accountConfigRuntime(runtime model.AuthRuntime) model.AccountConfigRuntime {
	if value, ok := runtime.(model.AccountConfigRuntime); ok {
		return value
	}
	return nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/templates", s.handleTemplates)
	mux.HandleFunc("GET /api/environment-profiles", s.handleEnvironmentProfiles)
	mux.HandleFunc("POST /api/environment-profiles", s.handleEnvironmentProfiles)
	mux.HandleFunc("PATCH /api/environment-profiles/{profile}", s.handleEnvironmentProfile)
	mux.HandleFunc("DELETE /api/environment-profiles/{profile}", s.handleEnvironmentProfile)
	mux.HandleFunc("POST /api/launch-preview", s.handleLaunchPreview)
	mux.HandleFunc("GET /api/accounts", s.handleAccounts)
	mux.HandleFunc("POST /api/accounts", s.handleCreateAccount)
	mux.HandleFunc("PATCH /api/accounts/{account}", s.handleUpdateAccount)
	mux.HandleFunc("POST /api/accounts/{account}/probe", s.handleProbeAccount)
	mux.HandleFunc("GET /api/accounts/{account}/auth-status", s.handleAccountAuthStatus)
	mux.HandleFunc("POST /api/accounts/{account}/login-attempts", s.handleStartAccountLogin)
	mux.HandleFunc("GET /api/accounts/{account}/login-attempts/{attempt}", s.handleAccountLoginAttempt)
	mux.HandleFunc("POST /api/accounts/{account}/login-attempts/{attempt}/complete", s.handleCompleteAccountLogin)
	mux.HandleFunc("DELETE /api/accounts/{account}/login-attempts/{attempt}", s.handleCancelAccountLogin)
	mux.HandleFunc("POST /api/accounts/{account}/logout", s.handleAccountLogout)
	mux.HandleFunc("DELETE /api/accounts/{account}", s.handleDeleteAccount)
	mux.HandleFunc("GET /api/accounts/{account}/config", s.handleAccountConfigList)
	mux.HandleFunc("GET /api/accounts/{account}/config/{path}", s.handleAccountConfigFile)
	mux.HandleFunc("PUT /api/accounts/{account}/config/{path}", s.handleAccountConfigFile)
	mux.HandleFunc("DELETE /api/accounts/{account}/config/{path}", s.handleAccountConfigFile)
	mux.HandleFunc("GET /api/workspaces", s.handleWorkspaces)
	mux.HandleFunc("POST /api/workspaces", s.handleCreateWorkspace)
	mux.HandleFunc("GET /api/workspaces/{id}", s.handleWorkspace)
	mux.HandleFunc("GET /api/workspaces/{id}/stats", s.handleWorkspaceStats)
	mux.HandleFunc("POST /api/workspaces/{id}/start", s.handleStartWorkspace)
	mux.HandleFunc("POST /api/workspaces/{id}/stop", s.handleStopWorkspace)
	mux.HandleFunc("POST /api/workspaces/{id}/files", s.handleImportFiles)
	mux.HandleFunc("GET /api/workspaces/{id}/sessions", s.handleSessions)
	mux.HandleFunc("POST /api/workspaces/{id}/sessions", s.handleCreateSession)
	mux.HandleFunc("PATCH /api/workspaces/{id}/sessions/{session}", s.handleUpdateSession)
	mux.HandleFunc("POST /api/workspaces/{id}/sessions/{session}/restart", s.handleRestartSession)
	mux.HandleFunc("GET /api/workspaces/{id}/sessions/{session}/transcript", s.handleSessionTranscript)
	mux.HandleFunc("DELETE /api/workspaces/{id}/sessions/{session}", s.handleDeleteSession)
	mux.HandleFunc("DELETE /api/workspaces/{id}", s.handleDeleteWorkspace)
	mux.HandleFunc("/w/", s.handleWorkspaceProxy)
	mux.Handle("/", spaHandler(s.web))
	return recoverMiddleware(loggingMiddleware(s.logger, mux))
}

func (s *Server) Reconcile(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	if s.auth != nil {
		if err := s.auth.runtime.ReconcileAuth(ctx, time.Now().UTC()); err != nil {
			s.logger.Warn("reconcile account auth helpers", "error", err)
		}
	}
	for _, item := range s.store.List() {
		ws := s.migrateWorkspace(item)
		info, err := s.runtime.Status(ctx, ws.ContainerName)
		if err != nil {
			ws.Status = "missing"
			ws.Error = err.Error()
			markSessionsStopped(&ws)
		} else {
			ws.Status = info.Status
			ws.Error = ""
			if info.Status == "running" {
				s.ensureWorkspaceSessions(ctx, &ws)
			} else {
				markSessionsStopped(&ws)
			}
		}
		ws.UpdatedAt = time.Now().UTC()
		if err := s.store.Put(ws); err != nil {
			s.logger.Error("reconcile workspace", "id", ws.ID, "error", err)
		}
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "time": time.Now().UTC(), "proxy": true, "multi_session": true,
		"managed_accounts": true, "managed_browser_login": s.authAvailable(),
		"session_parameter_auth": true, "pi_agent": true, "opencode": true,
		"encrypted_credentials": true,
		"control_plane_auth": map[string]any{
			"enabled":              s.controlPlaneAuthEnabled,
			"trusted_network_only": !s.controlPlaneAuthEnabled,
		},
	})
}

func (s *Server) Close(ctx context.Context) {
	if s.auth != nil {
		s.auth.Close(ctx)
	}
}

func (s *Server) handleTemplates(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.templateList)
}

func (s *Server) handleWorkspaces(w http.ResponseWriter, r *http.Request) {
	items := s.store.List()
	for i := range items {
		items[i] = publicWorkspace(s.refreshWorkspace(r.Context(), s.migrateWorkspace(items[i]), false))
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleWorkspace(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.workspaceFromRequest(w, r)
	if !ok {
		return
	}
	ws = s.refreshWorkspace(r.Context(), s.migrateWorkspace(ws), false)
	writeJSON(w, http.StatusOK, publicWorkspace(ws))
}

func (s *Server) handleWorkspaceStats(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.workspaceFromRequest(w, r)
	if !ok {
		return
	}
	stats, err := s.runtime.ResourceStats(r.Context(), ws.ContainerName)
	if err != nil {
		s.logger.Warn("workspace resource stats failed", "workspace", ws.ID, "error", err)
		writeError(w, http.StatusBadGateway, "无法读取 Workspace 资源", nil)
		return
	}
	response := map[string]any{
		"workspace_id":    ws.ID,
		"container_state": stats.ContainerState,
		"live":            stats.Live,
		"configured": map[string]any{
			"cpu_cores":    ws.CPUs,
			"memory_bytes": int64(ws.MemoryMB) * 1024 * 1024,
		},
		"effective_limit": map[string]any{
			"cpu_cores":    stats.CPULimit,
			"memory_bytes": stats.MemoryLimit,
		},
		"usage":      nil,
		"sampled_at": stats.SampledAt,
	}
	if stats.Unavailable != "" {
		response["unavailable_reason"] = stats.Unavailable
	}
	if stats.Live {
		response["usage"] = map[string]any{
			"cpu_percent":    stats.CPUPercent,
			"memory_bytes":   stats.MemoryBytes,
			"memory_percent": stats.MemoryPercent,
			"pids":           stats.PIDs,
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleCreateWorkspace(w http.ResponseWriter, r *http.Request) {
	s.accountOperationMu.Lock()
	defer s.accountOperationMu.Unlock()
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "无法解析创建请求", err)
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}

	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" || len(name) > 80 {
		writeError(w, http.StatusBadRequest, "名称不能为空且不能超过 80 个字符", nil)
		return
	}
	environmentProfileID := strings.TrimSpace(r.FormValue("environment_profile_id"))
	var environmentProfile model.EnvironmentProfile
	if environmentProfileID != "" {
		var found bool
		environmentProfile, found = s.store.GetEnvironmentProfile(environmentProfileID)
		if !found {
			writeError(w, http.StatusBadRequest, "定制环境不存在", nil)
			return
		}
	}
	templateID := strings.TrimSpace(r.FormValue("template_id"))
	if environmentProfile.ID != "" {
		templateID = environmentProfile.BaseTemplateID
	}
	template, ok := s.templates[templateID]
	if !ok {
		writeError(w, http.StatusBadRequest, "未知环境模板", nil)
		return
	}
	agent := normalizeAgent(r.FormValue("agent"))
	accountID := strings.TrimSpace(r.FormValue("account_id"))
	if err := s.validateAccountSelection(agent, accountID); err != nil {
		writeError(w, http.StatusBadRequest, "账号选择无效", err)
		return
	}
	permissionMode, err := validateSessionPermissions(agent, r.FormValue("permission_mode"), false, "", "")
	if err != nil {
		writeError(w, http.StatusBadRequest, "权限配置无效", err)
		return
	}
	directInput := directAgentInputFromForm(r)
	directConfig, err := normalizeDirectConfig(agent, directInput, nil)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Agent 参数无效", err)
		return
	}
	defaultCPUs, defaultMemoryMB := 2.0, 4096
	if environmentProfile.ID != "" {
		defaultCPUs, defaultMemoryMB = environmentProfile.CPUs, environmentProfile.MemoryMB
	}
	cpus := clampFloat(parseFloat(r.FormValue("cpus"), defaultCPUs), 0.25, 32)
	memoryMB := clampInt(parseInt(r.FormValue("memory_mb"), defaultMemoryMB), 512, 131072)
	requestEnvironment, _, err := parseEnvironment(r.FormValue("environment"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "环境变量格式错误", err)
		return
	}
	environment := make(map[string]string, len(environmentProfile.Environment)+len(requestEnvironment))
	for key, value := range environmentProfile.Environment {
		environment[key] = value
	}
	for key, value := range requestEnvironment {
		environment[key] = value
	}
	envKeys := make([]string, 0, len(environment))
	for key := range environment {
		envKeys = append(envKeys, key)
	}
	sort.Strings(envKeys)
	setup := model.EnvironmentSetup{
		AptPackages: environmentProfile.AptPackages, NPMModules: environmentProfile.NPMModules,
		PythonPackages: environmentProfile.PythonPackages, InitScript: environmentProfile.InitScript,
	}
	initializer, canInitialize := s.runtime.(model.EnvironmentRuntime)
	if environmentSetupRequired(setup) && !canInitialize {
		writeError(w, http.StatusInternalServerError, "当前运行时不支持定制环境初始化", nil)
		return
	}

	id, err := randomID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "无法生成 Workspace ID", err)
		return
	}
	password, err := randomPassword()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "无法生成内部终端凭据", err)
		return
	}
	passwordCiphertext, err := s.keyring.Encrypt(password)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "无法加密内部终端凭据", err)
		return
	}
	containerName := "vibedock-" + safeDockerName(name) + "-" + id[:6]
	now := time.Now().UTC()
	ws := model.Workspace{
		ID: id, Name: name, TemplateID: templateID, Image: template.Image,
		ContainerName: containerName, Status: "creating", Source: detectSource(r),
		CreatedAt: now, UpdatedAt: now, Password: password,
		PasswordCiphertext: passwordCiphertext, PasswordHint: secretHint(password), CPUs: cpus,
		MemoryMB: memoryMB, EnvKeys: envKeys, EnvironmentProfileID: environmentProfile.ID,
		EnvironmentProfileName: environmentProfile.Name,
	}
	if err := s.store.Put(ws); err != nil {
		writeError(w, http.StatusInternalServerError, "无法保存 Workspace", err)
		return
	}

	info, err := s.runtime.Create(r.Context(), model.CreateRuntimeRequest{
		ID: id, Name: safeDockerName(name), Image: template.Image, ContainerName: containerName,
		Password: password, CPUs: cpus, MemoryMB: memoryMB, Environment: environment,
	})
	if err != nil {
		ws.Status = "error"
		ws.Error = err.Error()
		ws.UpdatedAt = time.Now().UTC()
		_ = s.store.Put(ws)
		writeError(w, http.StatusInternalServerError, "创建 Docker 环境失败", err)
		return
	}
	ws.Status = info.Status

	if gitURL := strings.TrimSpace(r.FormValue("git_url")); gitURL != "" {
		if err := validateGitURL(gitURL); err != nil {
			_ = s.runtime.Delete(r.Context(), containerName, true)
			_ = s.store.Delete(id)
			writeError(w, http.StatusBadRequest, "Git 地址不合法", err)
			return
		}
		if err := s.runtime.CloneGit(r.Context(), containerName, gitURL); err != nil {
			ws.Error = "环境已创建，但 Git 克隆失败：" + err.Error()
		}
	}

	if err := s.importUploads(r.Context(), r.MultipartForm, containerName); err != nil {
		ws.Error = joinErrors(ws.Error, "文件导入失败："+err.Error())
	}
	if environmentSetupRequired(setup) {
		if err := initializer.InitializeEnvironment(r.Context(), containerName, setup); err != nil {
			_ = s.runtime.Delete(r.Context(), containerName, true)
			_ = s.store.Delete(id)
			writeError(w, http.StatusInternalServerError, "定制环境初始化失败", err)
			return
		}
	}

	session, sessionErr := s.newSession(&ws, defaultSessionName(agent, 1), agent, accountID, directConfig)
	if sessionErr != nil {
		ws.Error = joinErrors(ws.Error, "默认会话创建失败："+sessionErr.Error())
	} else {
		ws.Sessions = append(ws.Sessions, session)
		ws.Sessions[0].PermissionMode = permissionMode
		if err := s.saveDirectSecret(session.ID, directInput); err != nil {
			ws.Sessions[0].Status = "error"
			ws.Sessions[0].Error = err.Error()
			ws.Error = joinErrors(ws.Error, "默认会话凭据保存失败："+err.Error())
		}
		runtimeReq, runtimeErr := s.runtimeSession(ws, ws.Sessions[0])
		if runtimeErr != nil {
			ws.Sessions[0].Status = "error"
			ws.Sessions[0].Error = runtimeErr.Error()
			ws.Error = joinErrors(ws.Error, "默认会话账号加载失败："+runtimeErr.Error())
		} else if err := s.runtime.EnsureSession(r.Context(), ws.ContainerName, runtimeReq); err != nil {
			ws.Sessions[0].Status = "error"
			ws.Sessions[0].Error = err.Error()
			ws.Error = joinErrors(ws.Error, "默认会话启动失败："+err.Error())
		}
	}
	ws.UpdatedAt = time.Now().UTC()
	if err := s.store.Put(ws); err != nil {
		writeError(w, http.StatusInternalServerError, "环境已创建，但状态保存失败", err)
		return
	}
	writeJSON(w, http.StatusCreated, publicWorkspace(ws))
}

func (s *Server) handleStartWorkspace(w http.ResponseWriter, r *http.Request) {
	s.accountOperationMu.Lock()
	defer s.accountOperationMu.Unlock()
	ws, ok := s.workspaceFromRequest(w, r)
	if !ok {
		return
	}
	ws = s.migrateWorkspace(ws)
	info, err := s.runtime.Start(r.Context(), ws.ContainerName)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "启动失败", err)
		return
	}
	ws.Status = info.Status
	ws.Error = ""
	s.ensureWorkspaceSessions(r.Context(), &ws)
	ws.UpdatedAt = time.Now().UTC()
	_ = s.store.Put(ws)
	writeJSON(w, http.StatusOK, publicWorkspace(ws))
}

func (s *Server) handleStopWorkspace(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.workspaceFromRequest(w, r)
	if !ok {
		return
	}
	if err := s.runtime.Stop(r.Context(), ws.ContainerName); err != nil {
		writeError(w, http.StatusInternalServerError, "停止失败", err)
		return
	}
	ws.Status = "exited"
	markSessionsStopped(&ws)
	ws.UpdatedAt = time.Now().UTC()
	_ = s.store.Put(ws)
	writeJSON(w, http.StatusOK, publicWorkspace(ws))
}

func (s *Server) handleImportFiles(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.workspaceFromRequest(w, r)
	if !ok {
		return
	}
	if ws.Status != "running" {
		writeError(w, http.StatusConflict, "请先启动 Workspace", nil)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "无法解析上传请求", err)
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	if err := s.importUploads(r.Context(), r.MultipartForm, ws.ContainerName); err != nil {
		writeError(w, http.StatusBadRequest, "文件导入失败", err)
		return
	}
	ws.UpdatedAt = time.Now().UTC()
	_ = s.store.Put(ws)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.workspaceFromRequest(w, r)
	if !ok {
		return
	}
	ws = s.refreshWorkspace(r.Context(), s.migrateWorkspace(ws), false)
	writeJSON(w, http.StatusOK, ws.Sessions)
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	s.accountOperationMu.Lock()
	defer s.accountOperationMu.Unlock()
	ws, ok := s.workspaceFromRequest(w, r)
	if !ok {
		return
	}
	ws = s.migrateWorkspace(ws)
	var input struct {
		Name                     string                    `json:"name"`
		Agent                    string                    `json:"agent"`
		AccountID                string                    `json:"account_id"`
		PermissionMode           string                    `json:"permission_mode"`
		DangerousSkipPermissions bool                      `json:"dangerous_skip_permissions"`
		DangerousConfirmation    string                    `json:"dangerous_confirmation"`
		DirectConfig             directAgentInput          `json:"direct_config"`
		LaunchProfile            *model.AgentLaunchProfile `json:"launch_profile"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误", err)
		return
	}
	agent := normalizeAgent(input.Agent)
	accountID := strings.TrimSpace(input.AccountID)
	if err := s.validateAccountSelection(agent, accountID); err != nil {
		writeError(w, http.StatusBadRequest, "账号选择无效", err)
		return
	}
	directConfig, err := normalizeDirectConfig(agent, input.DirectConfig, nil)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Agent 参数无效", err)
		return
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		name = defaultSessionName(agent, len(ws.Sessions)+1)
	}
	if len(name) > 80 {
		writeError(w, http.StatusBadRequest, "会话名称不能超过 80 个字符", nil)
		return
	}
	permissionMode, err := validateSessionPermissions(agent, input.PermissionMode, input.DangerousSkipPermissions, input.DangerousConfirmation, ws.ID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "权限配置无效", err)
		return
	}
	session, err := s.newSession(&ws, name, agent, accountID, directConfig)
	if err != nil {
		writeError(w, http.StatusConflict, "无法分配会话端口", err)
		return
	}
	session.PermissionMode = permissionMode
	session.DangerousSkipPermissions = input.DangerousSkipPermissions
	launchProfile, err := normalizeLaunchProfile(agent, input.LaunchProfile)
	if err != nil {
		writeError(w, http.StatusBadRequest, "启动参数无效", err)
		return
	}
	session.LaunchProfile = launchProfile
	if err := s.saveDirectSecret(session.ID, input.DirectConfig); err != nil {
		writeError(w, http.StatusInternalServerError, "无法保存 Session API Key", err)
		return
	}
	if ws.Status == "running" {
		runtimeReq, runtimeErr := s.runtimeSession(ws, session)
		if runtimeErr != nil {
			session.Status = "error"
			session.Error = runtimeErr.Error()
		} else if err := s.runtime.EnsureSession(r.Context(), ws.ContainerName, runtimeReq); err != nil {
			session.Status = "error"
			session.Error = err.Error()
		}
	}
	ws.Sessions = append(ws.Sessions, session)
	ws.UpdatedAt = time.Now().UTC()
	if err := s.store.Put(ws); err != nil {
		writeError(w, http.StatusInternalServerError, "会话已创建，但保存失败", err)
		return
	}
	writeJSON(w, http.StatusCreated, session)
}

func (s *Server) handleUpdateSession(w http.ResponseWriter, r *http.Request) {
	s.accountOperationMu.Lock()
	defer s.accountOperationMu.Unlock()
	ws, index, ok := s.sessionFromRequest(w, r)
	if !ok {
		return
	}
	var input struct {
		Name                     *string                   `json:"name"`
		Agent                    *string                   `json:"agent"`
		AccountID                *string                   `json:"account_id"`
		PermissionMode           *string                   `json:"permission_mode"`
		DangerousSkipPermissions *bool                     `json:"dangerous_skip_permissions"`
		DangerousConfirmation    string                    `json:"dangerous_confirmation"`
		DirectConfig             *directAgentInput         `json:"direct_config"`
		LaunchProfile            *model.AgentLaunchProfile `json:"launch_profile"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误", err)
		return
	}
	session := ws.Sessions[index]
	if input.Name != nil {
		name := strings.TrimSpace(*input.Name)
		if name == "" || len(name) > 80 {
			writeError(w, http.StatusBadRequest, "会话名称不能为空且不能超过 80 个字符", nil)
			return
		}
		session.Name = name
	}

	restartNeeded := false
	if input.Agent != nil {
		agent := normalizeAgent(*input.Agent)
		if agent != session.Agent {
			session.Agent = agent
			session.PermissionMode = ""
			session.DangerousSkipPermissions = false
			session.LaunchProfile = nil
			restartNeeded = true
		}
	}
	if input.LaunchProfile != nil {
		launchProfile, err := normalizeLaunchProfile(session.Agent, input.LaunchProfile)
		if err != nil {
			writeError(w, http.StatusBadRequest, "启动参数无效", err)
			return
		}
		if !launchProfilesEqual(session.LaunchProfile, launchProfile) {
			restartNeeded = true
		}
		session.LaunchProfile = launchProfile
	}
	if input.AccountID != nil {
		accountID := strings.TrimSpace(*input.AccountID)
		if accountID != session.AccountID {
			session.AccountID = accountID
			restartNeeded = true
		}
	}
	if input.PermissionMode != nil || input.DangerousSkipPermissions != nil {
		permissionMode := session.PermissionMode
		if permissionMode == "" && session.Agent == "claude" {
			permissionMode = "manual"
		}
		if input.PermissionMode != nil {
			permissionMode = *input.PermissionMode
		}
		dangerous := session.DangerousSkipPermissions
		if input.DangerousSkipPermissions != nil {
			dangerous = *input.DangerousSkipPermissions
		}
		validatedMode, err := validateSessionPermissions(session.Agent, permissionMode, dangerous, input.DangerousConfirmation, ws.ID)
		if err != nil {
			writeError(w, http.StatusBadRequest, "权限配置无效", err)
			return
		}
		if session.PermissionMode != validatedMode || session.DangerousSkipPermissions != dangerous {
			restartNeeded = true
		}
		session.PermissionMode = validatedMode
		session.DangerousSkipPermissions = dangerous
	}
	if session.Agent == "shell" {
		session.AccountID = ""
	}
	if isDirectAgent(session.Agent) {
		session.AccountID = ""
	}
	if isDirectAgent(session.Agent) {
		directConfig := session.DirectConfig
		var directInput directAgentInput
		if input.DirectConfig != nil || directConfig == nil {
			if input.DirectConfig != nil {
				directInput = *input.DirectConfig
			}
			var err error
			directConfig, err = normalizeDirectConfig(session.Agent, directInput, session.DirectConfig)
			if err != nil {
				writeError(w, http.StatusBadRequest, "Agent 参数无效", err)
				return
			}
		}
		if directConfig == nil {
			writeError(w, http.StatusBadRequest, "Agent 参数无效", errors.New("缺少 Session 参数"))
			return
		}
		if !directConfigsEqual(session.DirectConfig, directConfig) || (input.DirectConfig != nil && (input.DirectConfig.Secret != "" || input.DirectConfig.ClearSecret)) {
			restartNeeded = true
		}
		session.DirectConfig = directConfig
		if input.DirectConfig != nil {
			if err := s.saveDirectSecret(session.ID, *input.DirectConfig); err != nil {
				writeError(w, http.StatusInternalServerError, "无法保存 Session API Key", err)
				return
			}
		}
	} else {
		session.DirectConfig = nil
		_ = s.store.DeleteSessionSecret(session.ID)
	}
	if err := s.validateAccountSelection(session.Agent, session.AccountID); err != nil {
		writeError(w, http.StatusBadRequest, "账号选择无效", err)
		return
	}
	if restartNeeded && ws.Status == "running" {
		runtimeReq, err := s.runtimeSession(ws, session)
		if err != nil {
			writeError(w, http.StatusBadRequest, "加载账号失败", err)
			return
		}
		if err := s.runtime.RestartSession(r.Context(), ws.ContainerName, runtimeReq); err != nil {
			writeError(w, http.StatusInternalServerError, "切换 Agent 或账号失败", err)
			return
		}
		session.Status = "running"
		session.Error = ""
	}
	session.UpdatedAt = time.Now().UTC()
	ws.Sessions[index] = session
	ws.UpdatedAt = session.UpdatedAt
	if err := s.store.Put(ws); err != nil {
		writeError(w, http.StatusInternalServerError, "保存会话失败", err)
		return
	}
	writeJSON(w, http.StatusOK, session)
}

func (s *Server) handleRestartSession(w http.ResponseWriter, r *http.Request) {
	s.accountOperationMu.Lock()
	defer s.accountOperationMu.Unlock()
	ws, index, ok := s.sessionFromRequest(w, r)
	if !ok {
		return
	}
	if ws.Status != "running" {
		writeError(w, http.StatusConflict, "请先启动 Workspace", nil)
		return
	}
	session := ws.Sessions[index]
	runtimeReq, err := s.runtimeSession(ws, session)
	if err != nil {
		writeError(w, http.StatusBadRequest, "加载账号失败", err)
		return
	}
	if err := s.runtime.RestartSession(r.Context(), ws.ContainerName, runtimeReq); err != nil {
		writeError(w, http.StatusInternalServerError, "重启会话失败", err)
		return
	}
	session.Status = "running"
	session.Error = ""
	session.UpdatedAt = time.Now().UTC()
	ws.Sessions[index] = session
	ws.UpdatedAt = session.UpdatedAt
	_ = s.store.Put(ws)
	writeJSON(w, http.StatusOK, session)
}

func (s *Server) handleSessionTranscript(w http.ResponseWriter, r *http.Request) {
	ws, index, ok := s.sessionFromRequest(w, r)
	if !ok {
		return
	}
	lines := clampInt(parseInt(r.URL.Query().Get("lines"), 800), 20, maxTranscriptRows)
	text, err := s.runtime.CaptureSession(r.Context(), ws.ContainerName, ws.Sessions[index].ID, lines)
	if err != nil {
		writeError(w, http.StatusConflict, "无法读取会话输出", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"text": text, "lines": lines})
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	ws, index, ok := s.sessionFromRequest(w, r)
	if !ok {
		return
	}
	session := ws.Sessions[index]
	if ws.Status == "running" {
		if err := s.runtime.DeleteSession(r.Context(), ws.ContainerName, session.ID); err != nil {
			writeError(w, http.StatusInternalServerError, "删除会话失败", err)
			return
		}
	}
	ws.Sessions = append(ws.Sessions[:index], ws.Sessions[index+1:]...)
	_ = s.store.DeleteSessionSecret(session.ID)
	ws.UpdatedAt = time.Now().UTC()
	if err := s.store.Put(ws); err != nil {
		writeError(w, http.StatusInternalServerError, "清理会话记录失败", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeleteWorkspace(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.workspaceFromRequest(w, r)
	if !ok {
		return
	}
	purge := r.URL.Query().Get("purge") != "false"
	if err := s.runtime.Delete(r.Context(), ws.ContainerName, purge); err != nil {
		writeError(w, http.StatusInternalServerError, "删除失败", err)
		return
	}
	if err := s.store.Delete(ws.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "容器已删除，但记录清理失败", err)
		return
	}
	for _, session := range ws.Sessions {
		_ = s.store.DeleteSessionSecret(session.ID)
	}
	w.WriteHeader(http.StatusNoContent)
}

func allowWorkbenchEmbedding(header http.Header) {
	header.Del("X-Frame-Options")
	header.Del("Frame-Options")
	for _, key := range []string{"Content-Security-Policy", "Content-Security-Policy-Report-Only"} {
		value := header.Get(key)
		if value == "" {
			continue
		}
		directives := strings.Split(value, ";")
		kept := directives[:0]
		for _, directive := range directives {
			trimmed := strings.TrimSpace(directive)
			if strings.HasPrefix(strings.ToLower(trimmed), "frame-ancestors") {
				continue
			}
			if trimmed != "" {
				kept = append(kept, trimmed)
			}
		}
		if len(kept) == 0 {
			header.Del(key)
		} else {
			header.Set(key, strings.Join(kept, "; "))
		}
	}
}

func (s *Server) ensureSessionSingleflight(ctx context.Context, ws model.Workspace, session model.Session) error {
	key := ws.ID + ":" + session.ID
	s.ensureMu.Lock()
	if call, ok := s.ensureCalls[key]; ok {
		s.ensureMu.Unlock()
		select {
		case <-call.done:
			return call.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	call := &ensureSessionCall{done: make(chan struct{})}
	s.ensureCalls[key] = call
	s.ensureMu.Unlock()

	s.accountOperationMu.Lock()
	runtimeReq, err := s.runtimeSession(ws, session)
	if err == nil {
		err = s.runtime.EnsureSession(ctx, ws.ContainerName, runtimeReq)
	}
	s.accountOperationMu.Unlock()
	s.ensureMu.Lock()
	call.err = err
	close(call.done)
	delete(s.ensureCalls, key)
	s.ensureMu.Unlock()
	return err
}

func (s *Server) handleWorkspaceProxy(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/w/"), "/")
	if len(parts) < 2 || !idPattern.MatchString(parts[0]) {
		http.NotFound(w, r)
		return
	}
	ws, ok := s.store.Get(parts[0])
	if !ok {
		http.NotFound(w, r)
		return
	}
	ws = s.migrateWorkspace(ws)
	info, statusErr := s.runtime.Status(r.Context(), ws.ContainerName)
	if statusErr != nil || info.Status != "running" {
		writeError(w, http.StatusConflict, "Workspace 未运行", statusErr)
		return
	}

	service := parts[1]
	var port int
	var prefix string
	stripPrefix := true
	injectTerminalAuth := false

	switch service {
	case "code":
		prefix = "/w/" + ws.ID + "/code"
		port = codeServerPort
	case "terminal":
		if len(parts) < 3 || !idPattern.MatchString(parts[2]) {
			http.NotFound(w, r)
			return
		}
		session, found := findSession(ws, parts[2])
		if !found {
			http.NotFound(w, r)
			return
		}
		if err := s.ensureSessionSingleflight(r.Context(), ws, session); err != nil {
			writeError(w, http.StatusBadGateway, "Session 启动失败", err)
			return
		}
		prefix = "/w/" + ws.ID + "/terminal/" + session.ID
		port = session.Port
		stripPrefix = false
		injectTerminalAuth = true
	case "port":
		if len(parts) < 3 {
			http.NotFound(w, r)
			return
		}
		port = parseInt(parts[2], 0)
		if port < 1 || port > 65535 {
			writeError(w, http.StatusBadRequest, "端口不合法", nil)
			return
		}
		prefix = "/w/" + ws.ID + "/port/" + strconv.Itoa(port)
	default:
		http.NotFound(w, r)
		return
	}

	if r.URL.Path == prefix {
		target := prefix + "/"
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, target, http.StatusTemporaryRedirect)
		return
	}
	if !strings.HasPrefix(r.URL.Path, prefix+"/") {
		http.NotFound(w, r)
		return
	}

	targetString, err := s.runtime.ProxyTarget(r.Context(), ws.ContainerName, port)
	if err != nil {
		writeError(w, http.StatusBadGateway, "Workspace 服务不可达", err)
		return
	}
	target, err := url.Parse(targetString)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "代理目标无效", err)
		return
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalHost := req.Host
		originalDirector(req)
		req.Host = originalHost
		req.Header.Set("X-Forwarded-Host", originalHost)
		req.Header.Set("X-Forwarded-Prefix", prefix)
		if stripPrefix {
			path := strings.TrimPrefix(req.URL.Path, prefix)
			if path == "" {
				path = "/"
			}
			req.URL.Path = path
			req.URL.RawPath = ""
		}
		if injectTerminalAuth {
			token := base64.StdEncoding.EncodeToString([]byte("vibe:" + ws.Password))
			req.Header.Set("Authorization", "Basic "+token)
		}
	}
	proxy.ModifyResponse = func(resp *http.Response) error {
		// Workspace tools are intentionally rendered inside the same-origin
		// VibeDock workbench. Development servers frequently send DENY or a
		// restrictive frame-ancestors directive by default, so preview responses
		// need a narrowly scoped frame-header rewrite at this proxy boundary.
		if service == "port" {
			allowWorkbenchEmbedding(resp.Header)
		}
		if service != "code" && service != "port" {
			return nil
		}
		if location := resp.Header.Get("Location"); strings.HasPrefix(location, "/") {
			resp.Header.Set("Location", prefix+location)
		}
		cookies := resp.Header.Values("Set-Cookie")
		if len(cookies) > 0 {
			resp.Header.Del("Set-Cookie")
			for _, cookie := range cookies {
				cookie = strings.Replace(cookie, "Path=/", "Path="+prefix+"/", 1)
				resp.Header.Add("Set-Cookie", cookie)
			}
		}
		return nil
	}
	proxy.ErrorHandler = func(rw http.ResponseWriter, req *http.Request, proxyErr error) {
		s.logger.Warn("workspace proxy failed", "workspace", ws.ID, "service", service, "error", proxyErr)
		writeError(rw, http.StatusBadGateway, "Workspace 服务连接失败", proxyErr)
	}
	proxy.ServeHTTP(w, r)
}

func (s *Server) refreshWorkspace(ctx context.Context, ws model.Workspace, ensure bool) model.Workspace {
	info, err := s.runtime.Status(ctx, ws.ContainerName)
	if err != nil {
		ws.Status = "missing"
		ws.Error = err.Error()
		markSessionsStopped(&ws)
		return ws
	}
	ws.Status = info.Status
	if info.Status != "running" {
		markSessionsStopped(&ws)
		return ws
	}
	for i := range ws.Sessions {
		runtimeReq, runtimeErr := s.runtimeSession(ws, ws.Sessions[i])
		if runtimeErr != nil {
			ws.Sessions[i].Status = "error"
			ws.Sessions[i].Error = runtimeErr.Error()
			continue
		}
		if ensure {
			if err := s.runtime.EnsureSession(ctx, ws.ContainerName, runtimeReq); err != nil {
				ws.Sessions[i].Status = "error"
				ws.Sessions[i].Error = err.Error()
				continue
			}
		}
		status, err := s.runtime.SessionStatus(ctx, ws.ContainerName, ws.Sessions[i].ID)
		if err != nil {
			ws.Sessions[i].Status = "error"
			ws.Sessions[i].Error = err.Error()
		} else {
			ws.Sessions[i].Status = status
			ws.Sessions[i].Error = ""
		}
	}
	return ws
}

func (s *Server) ensureWorkspaceSessions(ctx context.Context, ws *model.Workspace) {
	for i := range ws.Sessions {
		runtimeReq, runtimeErr := s.runtimeSession(*ws, ws.Sessions[i])
		if runtimeErr != nil {
			ws.Sessions[i].Status = "error"
			ws.Sessions[i].Error = runtimeErr.Error()
			continue
		}
		if err := s.runtime.EnsureSession(ctx, ws.ContainerName, runtimeReq); err != nil {
			ws.Sessions[i].Status = "error"
			ws.Sessions[i].Error = err.Error()
			continue
		}
		ws.Sessions[i].Status = "running"
		ws.Sessions[i].Error = ""
		ws.Sessions[i].UpdatedAt = time.Now().UTC()
	}
}

func (s *Server) migrateWorkspace(ws model.Workspace) model.Workspace {
	changed := false
	if ws.Password == "" && ws.PasswordCiphertext != "" {
		if password, err := s.keyring.Decrypt(ws.PasswordCiphertext); err == nil {
			ws.Password = password
		}
	}
	if ws.Password == "" && ws.LegacyPassword != "" {
		if ciphertext, err := s.keyring.Encrypt(ws.LegacyPassword); err == nil {
			ws.Password = ws.LegacyPassword
			ws.PasswordCiphertext = ciphertext
			ws.PasswordHint = secretHint(ws.Password)
			ws.LegacyPassword = ""
			changed = true
		}
	}
	usedPorts := map[int]bool{}
	for i := range ws.Sessions {
		if ws.Sessions[i].Port == 0 || usedPorts[ws.Sessions[i].Port] {
			ws.Sessions[i].Port = nextFreePort(usedPorts)
			changed = true
		}
		usedPorts[ws.Sessions[i].Port] = true
		if ws.Sessions[i].TmuxName == "" {
			ws.Sessions[i].TmuxName = "vd-" + ws.Sessions[i].ID
			changed = true
		}
	}
	if len(ws.Sessions) == 0 {
		agent := normalizeAgent(ws.Agent)
		session, err := s.newSession(&ws, defaultSessionName(agent, 1), agent, "", nil)
		if err == nil {
			ws.Sessions = []model.Session{session}
			changed = true
		}
	}
	if ws.Agent != "" || ws.VscodePort != 0 || ws.TerminalPort != 0 {
		ws.Agent = ""
		ws.VscodePort = 0
		ws.TerminalPort = 0
		changed = true
	}
	if changed {
		ws.UpdatedAt = time.Now().UTC()
		_ = s.store.Put(ws)
	}
	return ws
}

func (s *Server) newSession(ws *model.Workspace, name, agent, accountID string, directConfig *model.DirectAgentConfig) (model.Session, error) {
	id, err := randomID()
	if err != nil {
		return model.Session{}, err
	}
	used := make(map[int]bool, len(ws.Sessions))
	for _, session := range ws.Sessions {
		used[session.Port] = true
	}
	port := nextFreePort(used)
	if port == 0 {
		return model.Session{}, errors.New("Workspace 会话数量已达到上限")
	}
	now := time.Now().UTC()
	status := "stopped"
	if ws.Status == "running" || ws.Status == "creating" {
		status = "running"
	}
	return model.Session{
		ID: id, Name: name, Agent: normalizeAgent(agent), AccountID: accountID, DirectConfig: directConfig, TmuxName: "vd-" + id,
		Port: port, Status: status, CreatedAt: now, UpdatedAt: now,
	}, nil
}

func nextFreePort(used map[int]bool) int {
	for port := firstSessionPort; port <= lastSessionPort; port++ {
		if !used[port] {
			return port
		}
	}
	return 0
}

func defaultSessionName(agent string, number int) string {
	label := map[string]string{"codex": "Codex", "claude": "Claude Code", "pi": "Pi", "opencode": "OpenCode", "shell": "Shell"}[normalizeAgent(agent)]
	return fmt.Sprintf("%s %d", label, number)
}

func markSessionsStopped(ws *model.Workspace) {
	for i := range ws.Sessions {
		ws.Sessions[i].Status = "stopped"
		ws.Sessions[i].Error = ""
	}
}

func findSession(ws model.Workspace, id string) (model.Session, bool) {
	for _, session := range ws.Sessions {
		if session.ID == id {
			return session, true
		}
	}
	return model.Session{}, false
}

func publicWorkspace(ws model.Workspace) model.Workspace {
	ws.Password = ""
	ws.LegacyPassword = ""
	ws.PasswordCiphertext = ""
	ws.PasswordHint = ""
	return ws
}

func (s *Server) workspaceFromRequest(w http.ResponseWriter, r *http.Request) (model.Workspace, bool) {
	id := r.PathValue("id")
	if !idPattern.MatchString(id) {
		writeError(w, http.StatusBadRequest, "Workspace ID 不合法", nil)
		return model.Workspace{}, false
	}
	ws, ok := s.store.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "Workspace 不存在", nil)
		return model.Workspace{}, false
	}
	return ws, true
}

func (s *Server) sessionFromRequest(w http.ResponseWriter, r *http.Request) (model.Workspace, int, bool) {
	ws, ok := s.workspaceFromRequest(w, r)
	if !ok {
		return model.Workspace{}, 0, false
	}
	ws = s.migrateWorkspace(ws)
	sessionID := r.PathValue("session")
	if !idPattern.MatchString(sessionID) {
		writeError(w, http.StatusBadRequest, "Session ID 不合法", nil)
		return model.Workspace{}, 0, false
	}
	for index := range ws.Sessions {
		if ws.Sessions[index].ID == sessionID {
			return ws, index, true
		}
	}
	writeError(w, http.StatusNotFound, "Session 不存在", nil)
	return model.Workspace{}, 0, false
}

func (s *Server) importUploads(ctx context.Context, form *multipart.Form, containerName string) error {
	if form == nil || len(form.File["files"]) == 0 {
		return nil
	}
	tempDir, err := os.MkdirTemp("", "vibedock-upload-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tempDir)

	for _, header := range form.File["files"] {
		if err := extractUploadedFile(header, tempDir); err != nil {
			return err
		}
	}
	return s.runtime.CopyDirectory(ctx, containerName, tempDir)
}

func extractUploadedFile(header *multipart.FileHeader, root string) error {
	file, err := header.Open()
	if err != nil {
		return err
	}
	defer file.Close()

	filename := sanitizeRelativePath(header.Filename)
	if filename == "" {
		return errors.New("上传文件名为空")
	}
	if strings.HasSuffix(strings.ToLower(filename), ".zip") {
		tmp, err := os.CreateTemp("", "vibedock-*.zip")
		if err != nil {
			return err
		}
		tmpName := tmp.Name()
		defer os.Remove(tmpName)
		if _, err := io.Copy(tmp, file); err != nil {
			tmp.Close()
			return err
		}
		if err := tmp.Close(); err != nil {
			return err
		}
		return extractZip(tmpName, root)
	}
	destination := filepath.Join(root, filepath.FromSlash(filename))
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, file)
	return err
}

func extractZip(zipPath, root string) error {
	reader, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer reader.Close()
	if len(reader.File) > maxZipFiles {
		return fmt.Errorf("ZIP 文件数量超过限制：%d", maxZipFiles)
	}
	var extractedBytes int64
	for _, item := range reader.File {
		name := sanitizeRelativePath(item.Name)
		if name == "" {
			continue
		}
		destination := filepath.Join(root, filepath.FromSlash(name))
		if item.FileInfo().IsDir() {
			if err := os.MkdirAll(destination, 0o755); err != nil {
				return err
			}
			continue
		}
		if item.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return err
		}
		if item.UncompressedSize64 > uint64(maxExtractBytes-extractedBytes) {
			return fmt.Errorf("ZIP 解压后总量超过 %d MiB", maxExtractBytes>>20)
		}
		src, err := item.Open()
		if err != nil {
			return err
		}
		mode := item.Mode().Perm()
		if mode == 0 {
			mode = 0o644
		}
		dst, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
		if err != nil {
			src.Close()
			return err
		}
		written, copyErr := io.Copy(dst, io.LimitReader(src, maxExtractBytes-extractedBytes+1))
		extractedBytes += written
		closeErr := errors.Join(src.Close(), dst.Close())
		if copyErr != nil {
			return copyErr
		}
		if extractedBytes > maxExtractBytes {
			return fmt.Errorf("ZIP 解压后总量超过 %d MiB", maxExtractBytes>>20)
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func sanitizeRelativePath(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = strings.TrimPrefix(name, "/")
	clean := filepath.ToSlash(filepath.Clean(name))
	if clean == "." || clean == "" || clean == ".." || strings.HasPrefix(clean, "../") {
		return ""
	}
	return clean
}

func parseEnvironment(raw string) (map[string]string, []string, error) {
	values := make(map[string]string)
	keys := make([]string, 0)
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || !envKeyPattern.MatchString(key) {
			return nil, nil, fmt.Errorf("无效环境变量：%q", line)
		}
		if _, exists := values[key]; !exists {
			keys = append(keys, key)
		}
		values[key] = value
	}
	return values, keys, nil
}

func environmentSetupRequired(setup model.EnvironmentSetup) bool {
	return len(setup.AptPackages) > 0 || len(setup.NPMModules) > 0 || len(setup.PythonPackages) > 0 || strings.TrimSpace(setup.InitScript) != ""
}

func validateGitURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return errors.New("Git 地址不能为空")
	}
	if strings.HasPrefix(raw, "git@") {
		if strings.Contains(raw, " ") || strings.Contains(raw, "@/") {
			return errors.New("Git SSH 地址不合法")
		}
		return nil
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if parsed.Scheme != "https" && parsed.Scheme != "ssh" {
		return errors.New("仅支持 https、ssh 或 git@ 地址")
	}
	if parsed.Host == "" {
		return errors.New("Git 地址缺少主机名")
	}
	if parsed.User != nil {
		return errors.New("Git 地址不能包含用户名或密码，请使用安全的凭据配置")
	}
	return nil
}

func validateSessionPermissions(agent, mode string, dangerous bool, confirmation, workspaceID string) (string, error) {
	agent = normalizeAgent(agent)
	mode = strings.TrimSpace(mode)
	if mode == "" {
		mode = "manual"
	}
	if agent != "claude" {
		if mode != "manual" || dangerous {
			return "", errors.New("当前只有 Claude Code 支持会话权限模式")
		}
		return "", nil
	}
	switch mode {
	case "manual", "acceptEdits", "auto", "dontAsk", "plan":
	case "bypassPermissions":
		dangerous = true
	default:
		return "", errors.New("Claude 权限模式不受支持")
	}
	if dangerous {
		if workspaceID == "" || confirmation != workspaceID {
			return "", errors.New("跳过权限检查需要输入当前 Workspace ID 进行确认")
		}
		return "bypassPermissions", nil
	}
	return mode, nil
}

func normalizeAgent(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "codex":
		return "codex"
	case "claude", "claude-code":
		return "claude"
	case "pi", "pi-agent":
		return "pi"
	case "opencode", "open-code":
		return "opencode"
	default:
		return "shell"
	}
}

func detectSource(r *http.Request) string {
	if strings.TrimSpace(r.FormValue("git_url")) != "" {
		return "git"
	}
	if r.MultipartForm != nil && len(r.MultipartForm.File["files"]) > 0 {
		return "upload"
	}
	return "empty"
}

func randomID() (string, error) {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func randomPassword() (string, error) {
	buf := make([]byte, 18)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func safeDockerName(value string) string {
	value = nameSanitizer.ReplaceAllString(value, "-")
	value = dockerNameTrim.ReplaceAllString(value, "")
	value = strings.ToLower(value)
	if value == "" {
		return "workspace"
	}
	if len(value) > 40 {
		value = value[:40]
	}
	return value
}

func parseInt(raw string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return fallback
	}
	return value
}

func parseFloat(raw string, fallback float64) float64 {
	value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return fallback
	}
	return value
}

func clampInt(value, min, max int) int {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

func clampFloat(value, min, max float64) float64 {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

func joinErrors(current, next string) string {
	if current == "" {
		return next
	}
	return current + "；" + next
}

func decodeJSON(r *http.Request, value any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	return decoder.Decode(value)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string, err error) {
	payload := map[string]any{"error": message}
	if err != nil {
		payload["detail"] = err.Error()
	}
	writeJSON(w, status, payload)
}

func spaHandler(web fs.FS) http.Handler {
	files := http.FileServer(http.FS(web))
	serveIndex := func(w http.ResponseWriter) {
		data, err := fs.ReadFile(web, "index.html")
		if err != nil {
			http.Error(w, "index unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(data)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(filepath.Clean(r.URL.Path), "/")
		if path == "." || path == "" || path == "index.html" {
			serveIndex(w)
			return
		}
		info, err := fs.Stat(web, path)
		if err != nil || info.IsDir() {
			serveIndex(w)
			return
		}
		r.URL.Path = "/" + path
		files.ServeHTTP(w, r)
	})
}

func loggingMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, r)
		logger.Info("http request", "method", r.Method, "path", r.URL.Path, "duration", time.Since(started))
	})
}

func recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				writeError(w, http.StatusInternalServerError, "服务器内部错误", fmt.Errorf("%v", recovered))
			}
		}()
		next.ServeHTTP(w, r)
	})
}
