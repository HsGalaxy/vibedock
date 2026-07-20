package app

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"vibedock/internal/model"
)

const (
	maxEnvironmentProfiles = 64
	maxEnvironmentPackages = 50
	maxEnvironmentScript   = 16 << 10
	maxEnvironmentVars     = 40
)

var (
	environmentPackagePattern = regexp.MustCompile(`^[A-Za-z0-9@][A-Za-z0-9@._+:/~<>=!,*-]{0,119}$`)
	environmentPythonPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,79}(\[[A-Za-z0-9_,.-]+\])?([<>=!~][A-Za-z0-9.*+!<>=~.-]*)?$`)
)

type environmentProfileInput struct {
	Name           string            `json:"name"`
	Description    string            `json:"description"`
	Icon           string            `json:"icon"`
	BaseTemplateID string            `json:"base_template_id"`
	CPUs           float64           `json:"cpus"`
	MemoryMB       int               `json:"memory_mb"`
	AptPackages    []string          `json:"apt_packages"`
	NPMModules     []string          `json:"npm_modules"`
	PythonPackages []string          `json:"python_packages"`
	Environment    map[string]string `json:"environment"`
	InitScript     string            `json:"init_script"`
}

func (s *Server) handleEnvironmentProfiles(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.store.ListEnvironmentProfiles())
	case http.MethodPost:
		var input environmentProfileInput
		if err := decodeJSON(r, &input); err != nil {
			writeError(w, http.StatusBadRequest, "环境配置格式无效", err)
			return
		}
		if len(s.store.ListEnvironmentProfiles()) >= maxEnvironmentProfiles {
			writeError(w, http.StatusBadRequest, "环境配置数量已达到上限", nil)
			return
		}
		profile, err := s.normalizeEnvironmentProfile(input, model.EnvironmentProfile{})
		if err != nil {
			writeError(w, http.StatusBadRequest, "环境配置无效", err)
			return
		}
		if err := s.store.PutEnvironmentProfile(profile); err != nil {
			writeError(w, http.StatusInternalServerError, "保存环境配置失败", err)
			return
		}
		writeJSON(w, http.StatusCreated, profile)
	default:
		w.Header().Set("Allow", "GET, POST")
		writeError(w, http.StatusMethodNotAllowed, "不支持的环境配置操作", nil)
	}
}

func (s *Server) handleEnvironmentProfile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("profile")
	if !idPattern.MatchString(id) {
		writeError(w, http.StatusBadRequest, "环境配置 ID 不合法", nil)
		return
	}
	profile, ok := s.store.GetEnvironmentProfile(id)
	if !ok {
		writeError(w, http.StatusNotFound, "环境配置不存在", nil)
		return
	}
	switch r.Method {
	case http.MethodPatch:
		var input environmentProfileInput
		if err := decodeJSON(r, &input); err != nil {
			writeError(w, http.StatusBadRequest, "环境配置格式无效", err)
			return
		}
		updated, err := s.normalizeEnvironmentProfile(input, profile)
		if err != nil {
			writeError(w, http.StatusBadRequest, "环境配置无效", err)
			return
		}
		if err := s.store.PutEnvironmentProfile(updated); err != nil {
			writeError(w, http.StatusInternalServerError, "保存环境配置失败", err)
			return
		}
		writeJSON(w, http.StatusOK, updated)
	case http.MethodDelete:
		if err := s.store.DeleteEnvironmentProfile(id); err != nil {
			writeError(w, http.StatusInternalServerError, "删除环境配置失败", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "PATCH, DELETE")
		writeError(w, http.StatusMethodNotAllowed, "不支持的环境配置操作", nil)
	}
}

func (s *Server) normalizeEnvironmentProfile(input environmentProfileInput, existing model.EnvironmentProfile) (model.EnvironmentProfile, error) {
	name := strings.TrimSpace(input.Name)
	if name == "" || len([]rune(name)) > 80 {
		return model.EnvironmentProfile{}, errors.New("名称不能为空且不能超过 80 个字符")
	}
	description := strings.TrimSpace(input.Description)
	if len([]rune(description)) > 240 {
		return model.EnvironmentProfile{}, errors.New("描述不能超过 240 个字符")
	}
	icon := strings.TrimSpace(input.Icon)
	if icon == "" {
		icon = "◇"
	}
	if len([]rune(icon)) > 8 {
		return model.EnvironmentProfile{}, errors.New("图标不能超过 8 个字符")
	}
	baseTemplateID := strings.TrimSpace(input.BaseTemplateID)
	if _, ok := s.templates[baseTemplateID]; !ok {
		return model.EnvironmentProfile{}, errors.New("基础模板不存在")
	}
	cpus := input.CPUs
	if cpus == 0 {
		cpus = 2
	}
	if cpus < 0.25 || cpus > 32 {
		return model.EnvironmentProfile{}, errors.New("CPU 必须在 0.25 到 32 之间")
	}
	memoryMB := input.MemoryMB
	if memoryMB == 0 {
		memoryMB = 4096
	}
	if memoryMB < 512 || memoryMB > 131072 {
		return model.EnvironmentProfile{}, errors.New("内存必须在 512MB 到 131072MB 之间")
	}
	apt, err := normalizePackageList(input.AptPackages, false)
	if err != nil {
		return model.EnvironmentProfile{}, fmt.Errorf("APT 包：%w", err)
	}
	npm, err := normalizePackageList(input.NPMModules, false)
	if err != nil {
		return model.EnvironmentProfile{}, fmt.Errorf("npm 包：%w", err)
	}
	python, err := normalizePackageList(input.PythonPackages, true)
	if err != nil {
		return model.EnvironmentProfile{}, fmt.Errorf("Python 包：%w", err)
	}
	environment, err := normalizeProfileEnvironment(input.Environment)
	if err != nil {
		return model.EnvironmentProfile{}, err
	}
	if len(input.InitScript) > maxEnvironmentScript {
		return model.EnvironmentProfile{}, fmt.Errorf("初始化脚本不能超过 %d 字节", maxEnvironmentScript)
	}
	if strings.ContainsRune(input.InitScript, '\x00') {
		return model.EnvironmentProfile{}, errors.New("初始化脚本包含无效字符")
	}
	id := existing.ID
	createdAt := existing.CreatedAt
	if id == "" {
		var idErr error
		id, idErr = randomID()
		if idErr != nil {
			return model.EnvironmentProfile{}, idErr
		}
		createdAt = time.Now().UTC()
	}
	return model.EnvironmentProfile{
		ID: id, Name: name, Description: description, Icon: icon, BaseTemplateID: baseTemplateID,
		CPUs: cpus, MemoryMB: memoryMB, AptPackages: apt, NPMModules: npm, PythonPackages: python,
		Environment: environment, InitScript: input.InitScript, CreatedAt: createdAt, UpdatedAt: time.Now().UTC(),
	}, nil
}

func normalizePackageList(values []string, python bool) ([]string, error) {
	if len(values) > maxEnvironmentPackages {
		return nil, fmt.Errorf("最多 %d 个包", maxEnvironmentPackages)
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		if value == "" {
			continue
		}
		if len(value) > 120 || strings.Contains(value, "://") || strings.ContainsAny(value, "'\";\n\r") {
			return nil, fmt.Errorf("包名不合法：%q", value)
		}
		if python {
			if !environmentPythonPattern.MatchString(value) {
				return nil, fmt.Errorf("包名不合法：%q", value)
			}
		} else if !environmentPackagePattern.MatchString(value) {
			return nil, fmt.Errorf("包名不合法：%q", value)
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out, nil
}

func normalizeProfileEnvironment(values map[string]string) (map[string]string, error) {
	if len(values) > maxEnvironmentVars {
		return nil, fmt.Errorf("最多 %d 个环境变量", maxEnvironmentVars)
	}
	out := make(map[string]string, len(values))
	for rawKey, value := range values {
		key := strings.TrimSpace(rawKey)
		upper := strings.ToUpper(key)
		if !envKeyPattern.MatchString(key) {
			return nil, fmt.Errorf("环境变量名不合法：%q", key)
		}
		if strings.HasPrefix(upper, "VIBEDOCK_") || strings.HasPrefix(upper, "WORKSPACE_") || strings.HasPrefix(upper, "SESSION_") {
			return nil, fmt.Errorf("不允许覆盖系统环境变量：%s", key)
		}
		for _, sensitive := range []string{"API_KEY", "TOKEN", "PASSWORD", "SECRET", "PRIVATE_KEY"} {
			if strings.Contains(upper, sensitive) {
				return nil, fmt.Errorf("环境配置不能保存敏感变量：%s", key)
			}
		}
		if len(value) > 4096 || strings.ContainsRune(value, '\x00') {
			return nil, fmt.Errorf("环境变量 %s 的值无效或过长", key)
		}
		out[key] = value
	}
	return out, nil
}
