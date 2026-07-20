package model

import (
	"context"
	"time"
)

type Template struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Image       string   `json:"image"`
	Tags        []string `json:"tags,omitempty"`
	Icon        string   `json:"icon,omitempty"`
}

type Session struct {
	ID                       string             `json:"id"`
	Name                     string             `json:"name"`
	Agent                    string             `json:"agent"`
	AccountID                string             `json:"account_id,omitempty"`
	DirectConfig             *DirectAgentConfig `json:"direct_config,omitempty"`
	TmuxName                 string             `json:"tmux_name"`
	Port                     int                `json:"port,omitempty"`
	Status                   string             `json:"status"`
	CreatedAt                time.Time          `json:"created_at"`
	UpdatedAt                time.Time          `json:"updated_at"`
	Error                    string             `json:"error,omitempty"`
	PermissionMode           string             `json:"permission_mode,omitempty"`
	DangerousSkipPermissions bool               `json:"dangerous_skip_permissions,omitempty"`
}

// DirectAgentConfig belongs to a single Session. It is used by agents that do
// not participate in the managed AccountProfile system (currently Pi and
// OpenCode). API keys are encrypted separately by the store; only the hint and
// presence flag are exposed through the API.
type DirectAgentConfig struct {
	Provider         string `json:"provider,omitempty"`
	BaseURL          string `json:"base_url,omitempty"`
	Model            string `json:"model,omitempty"`
	Protocol         string `json:"protocol,omitempty"`
	Thinking         string `json:"thinking,omitempty"`
	PermissionPolicy string `json:"permission_policy,omitempty"`
	AutoApprove      bool   `json:"auto_approve,omitempty"` // Legacy API compatibility.
	HasSecret        bool   `json:"has_secret,omitempty"`
	SecretHint       string `json:"secret_hint,omitempty"`
}

type Workspace struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	TemplateID    string    `json:"template_id"`
	Image         string    `json:"image"`
	ContainerName string    `json:"container_name"`
	Status        string    `json:"status"`
	Source        string    `json:"source"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	// Password is an internal credential used only for ttyd. It is retained for
	// backward compatibility with existing state files and is never serialized.
	Password           string    `json:"-"`
	LegacyPassword     string    `json:"password,omitempty"`
	PasswordCiphertext string    `json:"password_ciphertext,omitempty"`
	PasswordHint       string    `json:"password_hint,omitempty"`
	CPUs               float64   `json:"cpus"`
	MemoryMB           int       `json:"memory_mb"`
	EnvKeys            []string  `json:"env_keys,omitempty"`
	Sessions           []Session `json:"sessions,omitempty"`
	Error              string    `json:"error,omitempty"`

	// Legacy fields are retained only so state files from the first MVP can be
	// loaded and migrated. New code does not write or expose host port mappings.
	Agent        string `json:"agent,omitempty"`
	VscodePort   int    `json:"vscode_port,omitempty"`
	TerminalPort int    `json:"terminal_port,omitempty"`
}

// AccountProfile is a centrally managed Codex or Claude Code identity/provider.
// SecretCiphertext is persisted encrypted and must never be returned directly
// by an HTTP handler.
type AccountProfile struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	Agent            string            `json:"agent"`
	AuthType         string            `json:"auth_type"`
	BaseURL          string            `json:"base_url,omitempty"`
	Model            string            `json:"model,omitempty"`
	ExtraEnv         map[string]string `json:"extra_env,omitempty"`
	SecretCiphertext string            `json:"secret_ciphertext,omitempty"`
	SecretHint       string            `json:"secret_hint,omitempty"`
	CreatedAt        time.Time         `json:"created_at"`
	UpdatedAt        time.Time         `json:"updated_at"`
	LastUsedAt       *time.Time        `json:"last_used_at,omitempty"`
}

type AccountView struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Agent      string            `json:"agent"`
	AuthType   string            `json:"auth_type"`
	BaseURL    string            `json:"base_url,omitempty"`
	Model      string            `json:"model,omitempty"`
	ExtraEnv   map[string]string `json:"extra_env,omitempty"`
	HasSecret  bool              `json:"has_secret"`
	SecretHint string            `json:"secret_hint,omitempty"`
	CreatedAt  time.Time         `json:"created_at"`
	UpdatedAt  time.Time         `json:"updated_at"`
	LastUsedAt *time.Time        `json:"last_used_at,omitempty"`
}

type Runtime interface {
	Create(ctx Context, req CreateRuntimeRequest) (RuntimeInfo, error)
	Start(ctx Context, containerName string) (RuntimeInfo, error)
	Stop(ctx Context, containerName string) error
	Delete(ctx Context, containerName string, purgeVolumes bool) error
	Status(ctx Context, containerName string) (RuntimeInfo, error)
	CopyDirectory(ctx Context, containerName, sourceDir string) error
	CloneGit(ctx Context, containerName, gitURL string) error
	ProxyTarget(ctx Context, containerName string, port int) (string, error)
	EnsureSession(ctx Context, containerName string, req SessionRuntimeRequest) error
	RestartSession(ctx Context, containerName string, req SessionRuntimeRequest) error
	DeleteSession(ctx Context, containerName, sessionID string) error
	SessionStatus(ctx Context, containerName, sessionID string) (string, error)
	CaptureSession(ctx Context, containerName, sessionID string, lines int) (string, error)
	ResourceStats(ctx Context, containerName string) (RuntimeResourceStats, error)
}

type Context = context.Context

type CreateRuntimeRequest struct {
	ID            string
	Name          string
	Image         string
	ContainerName string
	Password      string
	CPUs          float64
	MemoryMB      int
	Environment   map[string]string
}

type SessionRuntimeRequest struct {
	WorkspaceID              string
	SessionID                string
	Agent                    string
	Port                     int
	Password                 string
	PermissionMode           string
	DangerousSkipPermissions bool
	Account                  AccountRuntime
	Direct                   DirectAgentRuntime
}

type AccountRuntime struct {
	ID       string
	Name     string
	Agent    string
	AuthType string
	BaseURL  string
	Model    string
	Secret   string
	ExtraEnv map[string]string
}

type DirectAgentRuntime struct {
	Provider         string
	BaseURL          string
	Model            string
	Protocol         string
	Thinking         string
	PermissionPolicy string
	AutoApprove      bool
	Secret           string
}

type RuntimeInfo struct {
	Status string
}

type RuntimeResourceStats struct {
	ContainerState string
	Live           bool
	CPUPercent     *float64
	MemoryBytes    *uint64
	MemoryPercent  *float64
	PIDs           *uint64
	MemoryLimit    *uint64
	CPULimit       *float64
	SampledAt      time.Time
	Unavailable    string
}
