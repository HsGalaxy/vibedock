package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"vibedock/internal/model"
)

type Runtime struct {
	Command string
	Network string
}

type containerInspect struct {
	State struct {
		Status  string `json:"Status"`
		Running bool   `json:"Running"`
	} `json:"State"`
	HostConfig struct {
		Memory    int64 `json:"Memory"`
		NanoCPUs  int64 `json:"NanoCpus"`
		CPUPeriod int64 `json:"CpuPeriod"`
		CPUQuota  int64 `json:"CpuQuota"`
	} `json:"HostConfig"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

func New(command, network string) *Runtime {
	if strings.TrimSpace(command) == "" {
		command = "docker"
	}
	if strings.TrimSpace(network) == "" {
		network = "vibedock"
	}
	return &Runtime{Command: command, Network: network}
}

func (r *Runtime) Create(ctx model.Context, req model.CreateRuntimeRequest) (model.RuntimeInfo, error) {
	cctx, cancel := context.WithTimeout(asContext(ctx), 2*time.Minute)
	defer cancel()

	if err := r.ensureNetwork(cctx); err != nil {
		return model.RuntimeInfo{}, err
	}
	workspaceVolume := "vibedock-ws-" + req.ID
	homeVolume := "vibedock-home-" + req.ID
	credentialsVolume := "vibedock-credentials"
	createdVolumes := make([]string, 0, 2)
	for _, volume := range []string{workspaceVolume, homeVolume, credentialsVolume} {
		if _, err := r.run(cctx, "volume", "create", volume); err != nil {
			for _, created := range createdVolumes {
				_, _ = r.run(cctx, "volume", "rm", created)
			}
			return model.RuntimeInfo{}, err
		}
		if volume != credentialsVolume {
			createdVolumes = append(createdVolumes, volume)
		}
	}
	rollback := func() {
		for _, volume := range createdVolumes {
			_, _ = r.run(cctx, "volume", "rm", volume)
		}
	}

	args := []string{
		"create",
		"--name", req.ContainerName,
		"--hostname", req.Name,
		"--network", r.Network,
		"--label", "vibedock.managed=true",
		"--label", "vibedock.workspace_id=" + req.ID,
		"--restart", "unless-stopped",
		"--memory", fmt.Sprintf("%dm", req.MemoryMB),
		"--cpus", strconv.FormatFloat(req.CPUs, 'f', -1, 64),
		"-v", workspaceVolume + ":/workspace",
		"-v", homeVolume + ":/home/coder",
		"-v", credentialsVolume + ":/vibedock/credentials",
		"-e", "WORKSPACE_ID=" + req.ID,
		"-e", "WORKSPACE_NAME=" + req.Name,
	}
	for key, value := range req.Environment {
		args = append(args, "-e", key+"="+value)
	}
	args = append(args, req.Image)

	if _, err := r.run(cctx, args...); err != nil {
		rollback()
		return model.RuntimeInfo{}, err
	}
	if _, err := r.run(cctx, "start", req.ContainerName); err != nil {
		_ = r.Delete(cctx, req.ContainerName, true)
		rollback()
		return model.RuntimeInfo{}, err
	}
	info, err := r.inspect(cctx, req.ContainerName)
	if err != nil {
		_ = r.Delete(cctx, req.ContainerName, true)
		rollback()
		return model.RuntimeInfo{}, err
	}
	return info, nil
}

func (r *Runtime) Start(ctx model.Context, containerName string) (model.RuntimeInfo, error) {
	cctx, cancel := context.WithTimeout(asContext(ctx), 45*time.Second)
	defer cancel()
	if err := r.ensureContainerNetwork(cctx, containerName); err != nil {
		return model.RuntimeInfo{}, err
	}
	if _, err := r.run(cctx, "start", containerName); err != nil && !strings.Contains(err.Error(), "is already running") {
		return model.RuntimeInfo{}, err
	}
	return r.inspect(cctx, containerName)
}

func (r *Runtime) Stop(ctx model.Context, containerName string) error {
	cctx, cancel := context.WithTimeout(asContext(ctx), 45*time.Second)
	defer cancel()
	_, err := r.run(cctx, "stop", "--time", "10", containerName)
	return err
}

func (r *Runtime) Delete(ctx model.Context, containerName string, purgeVolumes bool) error {
	cctx, cancel := context.WithTimeout(asContext(ctx), 45*time.Second)
	defer cancel()

	var volumes []string
	if purgeVolumes {
		mounts, err := r.run(cctx, "inspect", "-f", `{{range .Mounts}}{{if eq .Type "volume"}}{{println .Name}}{{end}}{{end}}`, containerName)
		if err == nil {
			for _, name := range strings.Fields(mounts) {
				if strings.HasPrefix(name, "vibedock-") && name != "vibedock-credentials" {
					volumes = append(volumes, name)
				}
			}
		}
	}

	_, err := r.run(cctx, "rm", "-f", containerName)
	if err != nil && !strings.Contains(err.Error(), "No such container") {
		return err
	}
	if purgeVolumes {
		for _, volume := range volumes {
			if _, volumeErr := r.run(cctx, "volume", "rm", volume); volumeErr != nil {
				return volumeErr
			}
		}
	}
	return nil
}

func (r *Runtime) Status(ctx model.Context, containerName string) (model.RuntimeInfo, error) {
	cctx, cancel := context.WithTimeout(asContext(ctx), 15*time.Second)
	defer cancel()
	if err := r.ensureContainerNetwork(cctx, containerName); err != nil {
		return model.RuntimeInfo{}, err
	}
	return r.inspect(cctx, containerName)
}

type dockerStats struct {
	CPUPercent    string `json:"CPUPerc"`
	MemoryPercent string `json:"MemPerc"`
	MemoryUsage   string `json:"MemUsage"`
	PIDs          string `json:"PIDs"`
}

func (r *Runtime) ResourceStats(ctx model.Context, containerName string) (model.RuntimeResourceStats, error) {
	cctx, cancel := context.WithTimeout(asContext(ctx), 10*time.Second)
	defer cancel()

	now := time.Now().UTC()
	inspect, err := r.inspectRaw(cctx, containerName)
	if err != nil {
		if isContainerMissing(err) {
			return model.RuntimeResourceStats{ContainerState: "missing", SampledAt: now, Unavailable: "container_missing"}, nil
		}
		return model.RuntimeResourceStats{}, err
	}
	result := resourceStatsFromInspect(inspect, now)
	if inspect.State.Status != "running" || !inspect.State.Running {
		result.Unavailable = "container_not_running"
		return result, nil
	}

	output, err := r.run(cctx, "stats", "--no-stream", "--format", "{{json .}}", containerName)
	if err != nil {
		latest, inspectErr := r.inspectRaw(cctx, containerName)
		if inspectErr == nil && (latest.State.Status != "running" || !latest.State.Running) {
			result = resourceStatsFromInspect(latest, time.Now().UTC())
			result.Unavailable = "container_not_running"
			return result, nil
		}
		if isContainerMissing(err) || isContainerMissing(inspectErr) {
			return model.RuntimeResourceStats{ContainerState: "missing", SampledAt: time.Now().UTC(), Unavailable: "container_missing"}, nil
		}
		return model.RuntimeResourceStats{}, err
	}
	parsed, err := parseDockerStats(output)
	if err != nil {
		return model.RuntimeResourceStats{}, err
	}
	result.Live = true
	result.CPUPercent = &parsed.CPUPercent
	result.MemoryBytes = &parsed.MemoryBytes
	result.MemoryPercent = &parsed.MemoryPercent
	result.PIDs = &parsed.PIDs
	if result.MemoryLimit == nil && parsed.MemoryLimit > 0 {
		result.MemoryLimit = &parsed.MemoryLimit
	}
	result.SampledAt = time.Now().UTC()
	return result, nil
}

func resourceStatsFromInspect(inspect containerInspect, sampledAt time.Time) model.RuntimeResourceStats {
	result := model.RuntimeResourceStats{ContainerState: strings.TrimSpace(inspect.State.Status), SampledAt: sampledAt}
	if inspect.HostConfig.Memory > 0 {
		memory := uint64(inspect.HostConfig.Memory)
		result.MemoryLimit = &memory
	}
	var cpus float64
	if inspect.HostConfig.NanoCPUs > 0 {
		cpus = float64(inspect.HostConfig.NanoCPUs) / 1e9
	} else if inspect.HostConfig.CPUQuota > 0 && inspect.HostConfig.CPUPeriod > 0 {
		cpus = float64(inspect.HostConfig.CPUQuota) / float64(inspect.HostConfig.CPUPeriod)
	}
	if cpus > 0 {
		result.CPULimit = &cpus
	}
	return result
}

type parsedDockerStats struct {
	CPUPercent    float64
	MemoryBytes   uint64
	MemoryLimit   uint64
	MemoryPercent float64
	PIDs          uint64
}

func parseDockerStats(output string) (parsedDockerStats, error) {
	decoder := json.NewDecoder(strings.NewReader(output))
	var raw dockerStats
	if err := decoder.Decode(&raw); err != nil {
		return parsedDockerStats{}, fmt.Errorf("decode docker stats: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return parsedDockerStats{}, errors.New("unexpected multiple docker stats results")
		}
		return parsedDockerStats{}, fmt.Errorf("decode docker stats trailer: %w", err)
	}
	cpu, err := parseDockerPercent(raw.CPUPercent)
	if err != nil {
		return parsedDockerStats{}, fmt.Errorf("parse CPU percentage: %w", err)
	}
	memoryPercent, err := parseDockerPercent(raw.MemoryPercent)
	if err != nil {
		return parsedDockerStats{}, fmt.Errorf("parse memory percentage: %w", err)
	}
	usedText, limitText, ok := strings.Cut(raw.MemoryUsage, "/")
	if !ok {
		return parsedDockerStats{}, fmt.Errorf("invalid memory usage %q", raw.MemoryUsage)
	}
	used, err := parseDockerBytes(strings.TrimSpace(usedText))
	if err != nil {
		return parsedDockerStats{}, fmt.Errorf("parse memory usage: %w", err)
	}
	limit, err := parseDockerBytes(strings.TrimSpace(limitText))
	if err != nil {
		return parsedDockerStats{}, fmt.Errorf("parse memory limit: %w", err)
	}
	pids, err := strconv.ParseUint(strings.TrimSpace(raw.PIDs), 10, 64)
	if err != nil {
		return parsedDockerStats{}, fmt.Errorf("parse PIDs: %w", err)
	}
	return parsedDockerStats{CPUPercent: cpu, MemoryBytes: used, MemoryLimit: limit, MemoryPercent: memoryPercent, PIDs: pids}, nil
}

func parseDockerPercent(value string) (float64, error) {
	value = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(value), "%"))
	if value == "" || strings.EqualFold(value, "N/A") {
		return 0, fmt.Errorf("percentage is unavailable")
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed < 0 {
		return 0, fmt.Errorf("invalid percentage %q", value)
	}
	return parsed, nil
}

func parseDockerBytes(value string) (uint64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, errors.New("empty byte value")
	}
	index := 0
	for index < len(value) && ((value[index] >= '0' && value[index] <= '9') || value[index] == '.') {
		index++
	}
	if index == 0 {
		return 0, fmt.Errorf("invalid byte value %q", value)
	}
	number, err := strconv.ParseFloat(value[:index], 64)
	if err != nil || math.IsNaN(number) || math.IsInf(number, 0) || number < 0 {
		return 0, fmt.Errorf("invalid byte value %q", value)
	}
	unit := strings.ToLower(strings.TrimSpace(value[index:]))
	multipliers := map[string]float64{
		"": 1, "b": 1,
		"kb": 1e3, "mb": 1e6, "gb": 1e9, "tb": 1e12, "pb": 1e15,
		"kib": 1 << 10, "mib": 1 << 20, "gib": 1 << 30, "tib": 1 << 40, "pib": 1 << 50,
	}
	multiplier, ok := multipliers[unit]
	if !ok {
		return 0, fmt.Errorf("unsupported byte unit %q", unit)
	}
	bytes := number * multiplier
	if bytes > math.MaxUint64 {
		return 0, fmt.Errorf("byte value overflows uint64")
	}
	return uint64(math.Round(bytes)), nil
}

func isContainerMissing(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "no such container")
}

func (r *Runtime) CopyDirectory(ctx model.Context, containerName, sourceDir string) error {
	cctx, cancel := context.WithTimeout(asContext(ctx), 5*time.Minute)
	defer cancel()
	if _, err := r.run(cctx, "cp", sourceDir+"/.", containerName+":/workspace"); err != nil {
		return err
	}
	_, err := r.run(cctx, "exec", "-u", "root", containerName, "chown", "-R", "coder:coder", "/workspace")
	return err
}

func (r *Runtime) CloneGit(ctx model.Context, containerName, gitURL string) error {
	cctx, cancel := context.WithTimeout(asContext(ctx), 10*time.Minute)
	defer cancel()
	_, err := r.run(cctx, "exec", "-u", "coder", "-w", "/workspace", containerName,
		"git", "clone", "--depth", "1", "--", gitURL, ".")
	return err
}

func (r *Runtime) ProxyTarget(ctx model.Context, containerName string, port int) (string, error) {
	if port < 1 || port > 65535 {
		return "", fmt.Errorf("invalid proxy port %d", port)
	}
	cctx, cancel := context.WithTimeout(asContext(ctx), 10*time.Second)
	defer cancel()
	if err := r.ensureContainerNetwork(cctx, containerName); err != nil {
		return "", err
	}
	inspect, err := r.inspectRaw(cctx, containerName)
	if err != nil {
		return "", err
	}
	if inspect.State.Status != "running" {
		return "", fmt.Errorf("workspace is not running")
	}
	ip := ""
	if network, ok := inspect.NetworkSettings.Networks[r.Network]; ok {
		ip = network.IPAddress
	}
	if ip == "" {
		for _, network := range inspect.NetworkSettings.Networks {
			if network.IPAddress != "" {
				ip = network.IPAddress
				break
			}
		}
	}
	if net.ParseIP(ip) == nil {
		return "", fmt.Errorf("workspace has no reachable container address")
	}
	return "http://" + net.JoinHostPort(ip, strconv.Itoa(port)), nil
}

func (r *Runtime) EnsureSession(ctx model.Context, containerName string, req model.SessionRuntimeRequest) error {
	cctx, cancel := context.WithTimeout(asContext(ctx), 30*time.Second)
	defer cancel()
	_, err := r.runWithInput(cctx, sessionBootstrapScript(req, false), "exec", "-i", "-u", "coder", "-e", "HOME=/home/coder", "-e", "NPM_CONFIG_PREFIX=/home/coder/.local", "-e", "PATH=/home/coder/.local/bin:/opt/vibedock-cli/node_modules/.bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", containerName, "bash", "-s")
	return err
}

func (r *Runtime) RestartSession(ctx model.Context, containerName string, req model.SessionRuntimeRequest) error {
	cctx, cancel := context.WithTimeout(asContext(ctx), 35*time.Second)
	defer cancel()
	_, err := r.runWithInput(cctx, sessionBootstrapScript(req, true), "exec", "-i", "-u", "coder", "-e", "HOME=/home/coder", "-e", "NPM_CONFIG_PREFIX=/home/coder/.local", "-e", "PATH=/home/coder/.local/bin:/opt/vibedock-cli/node_modules/.bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", containerName, "bash", "-s")
	return err
}

func (r *Runtime) DeleteSession(ctx model.Context, containerName, sessionID string) error {
	cctx, cancel := context.WithTimeout(asContext(ctx), 20*time.Second)
	defer cancel()
	_, err := r.run(cctx, "exec", "-u", "coder", "-e", "HOME=/home/coder", "-e", "SESSION_ID="+sessionID, containerName,
		"bash", "-lc", stopSessionScript+"\n"+`rm -f "$HOME/.vibedock/sessions/${SESSION_ID}.pid" "$HOME/.vibedock/sessions/${SESSION_ID}.log"`)
	return err
}

func (r *Runtime) SessionStatus(ctx model.Context, containerName, sessionID string) (string, error) {
	cctx, cancel := context.WithTimeout(asContext(ctx), 10*time.Second)
	defer cancel()
	_, err := r.run(cctx, "exec", "-u", "coder", "-e", "HOME=/home/coder", "-e", "SESSION_ID="+sessionID, containerName,
		"bash", "-lc", sessionStatusScript)
	if err != nil {
		return "stopped", nil
	}
	return "running", nil
}

func (r *Runtime) CaptureSession(ctx model.Context, containerName, sessionID string, lines int) (string, error) {
	if lines < 20 {
		lines = 20
	}
	if lines > 5000 {
		lines = 5000
	}
	cctx, cancel := context.WithTimeout(asContext(ctx), 10*time.Second)
	defer cancel()
	return r.run(cctx, "exec", "-u", "coder", "-e", "HOME=/home/coder", containerName, "tmux", "capture-pane", "-p", "-J",
		"-t", "vd-"+sessionID, "-S", "-"+strconv.Itoa(lines))
}

func sessionBootstrapScript(req model.SessionRuntimeRequest, restart bool) string {
	values := map[string]string{
		"WORKSPACE_ID":             req.WorkspaceID,
		"SESSION_ID":               req.SessionID,
		"AGENT":                    req.Agent,
		"SESSION_PORT":             strconv.Itoa(req.Port),
		"VIBEDOCK_PERMISSION_MODE": req.PermissionMode,
		"VIBEDOCK_DANGEROUS_SKIP":  strconv.FormatBool(req.DangerousSkipPermissions),
		// Workspace password is no longer used by code-server (auth is enforced by
		// the control-plane proxy). It remains available only to ttyd's session
		// bootstrap, which receives it through the private stdin path below.
		"WORKSPACE_PASSWORD":           req.Password,
		"VIBEDOCK_ACCOUNT_ID":          req.Account.ID,
		"VIBEDOCK_ACCOUNT_NAME":        req.Account.Name,
		"VIBEDOCK_AUTH_TYPE":           req.Account.AuthType,
		"VIBEDOCK_BASE_URL":            req.Account.BaseURL,
		"VIBEDOCK_MODEL":               req.Account.Model,
		"VIBEDOCK_PROVIDER_TOKEN":      req.Account.Secret,
		"VIBEDOCK_DIRECT_PROVIDER":     req.Direct.Provider,
		"VIBEDOCK_DIRECT_BASE_URL":     req.Direct.BaseURL,
		"VIBEDOCK_DIRECT_MODEL":        req.Direct.Model,
		"VIBEDOCK_DIRECT_PROTOCOL":     req.Direct.Protocol,
		"VIBEDOCK_DIRECT_THINKING":     req.Direct.Thinking,
		"VIBEDOCK_PERMISSION_POLICY":   req.Direct.PermissionPolicy,
		"VIBEDOCK_DIRECT_AUTO_APPROVE": strconv.FormatBool(req.Direct.AutoApprove),
		"VIBEDOCK_DIRECT_API_KEY":      req.Direct.Secret,
	}
	keys := make([]string, 0, len(req.Account.ExtraEnv))
	for key := range req.Account.ExtraEnv {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	values["VIBEDOCK_EXTRA_ENV_KEYS"] = strings.Join(keys, ",")
	for _, key := range keys {
		values[key] = req.Account.ExtraEnv[key]
	}

	ordered := make([]string, 0, len(values))
	for key := range values {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	var script strings.Builder
	script.WriteString("set -Eeuo pipefail\n")
	for _, key := range ordered {
		script.WriteString("export ")
		script.WriteString(key)
		script.WriteByte('=')
		script.WriteString(shellQuote(values[key]))
		script.WriteByte('\n')
	}
	if restart {
		script.WriteString(stopSessionScript)
		script.WriteByte('\n')
	}
	script.WriteString(ensureSessionScript)
	return script.String()
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func (r *Runtime) inspect(ctx context.Context, containerName string) (model.RuntimeInfo, error) {
	inspect, err := r.inspectRaw(ctx, containerName)
	if err != nil {
		return model.RuntimeInfo{}, err
	}
	return model.RuntimeInfo{Status: strings.TrimSpace(inspect.State.Status)}, nil
}

func (r *Runtime) inspectRaw(ctx context.Context, containerName string) (containerInspect, error) {
	output, err := r.run(ctx, "inspect", containerName)
	if err != nil {
		return containerInspect{}, err
	}
	var items []containerInspect
	if err := json.Unmarshal([]byte(output), &items); err != nil {
		return containerInspect{}, fmt.Errorf("decode docker inspect: %w", err)
	}
	if len(items) != 1 {
		return containerInspect{}, fmt.Errorf("unexpected docker inspect result")
	}
	return items[0], nil
}

func (r *Runtime) ensureNetwork(ctx context.Context) error {
	if _, err := r.run(ctx, "network", "inspect", r.Network); err == nil {
		return nil
	}
	if _, err := r.run(ctx, "network", "create", r.Network); err != nil {
		if _, inspectErr := r.run(ctx, "network", "inspect", r.Network); inspectErr != nil {
			return err
		}
	}
	return nil
}

func (r *Runtime) ensureContainerNetwork(ctx context.Context, containerName string) error {
	if err := r.ensureNetwork(ctx); err != nil {
		return err
	}
	inspect, err := r.inspectRaw(ctx, containerName)
	if err != nil {
		return err
	}
	if _, ok := inspect.NetworkSettings.Networks[r.Network]; ok {
		return nil
	}
	_, err = r.run(ctx, "network", "connect", r.Network, containerName)
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		return err
	}
	return nil
}

func (r *Runtime) run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, r.Command, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		visible := strings.Join(redactDockerArgs(args), " ")
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("docker command timed out: %s", visible)
		}
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return "", fmt.Errorf("docker %s: %s", visible, message)
	}
	return strings.TrimSpace(stdout.String()), nil
}

func (r *Runtime) runWithInput(ctx context.Context, input string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, r.Command, args...)
	cmd.Stdin = strings.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		visible := strings.Join(redactDockerArgs(args), " ")
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("docker command timed out: %s", visible)
		}
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return "", fmt.Errorf("docker %s: %s", visible, message)
	}
	return strings.TrimSpace(stdout.String()), nil
}

func redactDockerArgs(args []string) []string {
	out := append([]string(nil), args...)
	for i := 0; i < len(out)-1; i++ {
		if out[i] != "-e" && out[i] != "--env" {
			continue
		}
		key, _, ok := strings.Cut(out[i+1], "=")
		if ok && (strings.Contains(key, "TOKEN") || strings.Contains(key, "KEY") || strings.Contains(key, "SECRET") || strings.Contains(key, "PASSWORD")) {
			out[i+1] = key + "=<redacted>"
		}
	}
	return out
}

func asContext(ctx model.Context) context.Context {
	if c, ok := ctx.(context.Context); ok {
		return c
	}
	return context.Background()
}

const ensureSessionScript = `
set -Eeuo pipefail
state_dir="$HOME/.vibedock/sessions"
runtime_dir="/dev/shm/vibedock/sessions"
mkdir -p "$state_dir" "$runtime_dir"
chmod 700 "$runtime_dir"
tmux_name="vd-${SESSION_ID}"
pid_file="$state_dir/${SESSION_ID}.pid"
log_file="$state_dir/${SESSION_ID}.log"
env_file="$runtime_dir/${SESSION_ID}.env"
base_path="/w/${WORKSPACE_ID}/terminal/${SESSION_ID}"
umask 077

write_export() {
  local key="$1" value="$2"
  printf 'export %s=%q\n' "$key" "$value" >>"$env_file"
}
: >"$env_file"
write_export AGENT "$AGENT"
write_export VIBEDOCK_PERMISSION_MODE "${VIBEDOCK_PERMISSION_MODE:-manual}"
write_export VIBEDOCK_DANGEROUS_SKIP "${VIBEDOCK_DANGEROUS_SKIP:-false}"
write_export VIBEDOCK_ACCOUNT_ID "${VIBEDOCK_ACCOUNT_ID:-}"
write_export VIBEDOCK_ACCOUNT_NAME "${VIBEDOCK_ACCOUNT_NAME:-}"
write_export VIBEDOCK_AUTH_TYPE "${VIBEDOCK_AUTH_TYPE:-workspace}"
write_export VIBEDOCK_BASE_URL "${VIBEDOCK_BASE_URL:-}"
write_export VIBEDOCK_MODEL "${VIBEDOCK_MODEL:-}"
write_export VIBEDOCK_PROVIDER_TOKEN "${VIBEDOCK_PROVIDER_TOKEN:-}"
write_export VIBEDOCK_DIRECT_PROVIDER "${VIBEDOCK_DIRECT_PROVIDER:-}"
write_export VIBEDOCK_DIRECT_BASE_URL "${VIBEDOCK_DIRECT_BASE_URL:-}"
write_export VIBEDOCK_DIRECT_MODEL "${VIBEDOCK_DIRECT_MODEL:-}"
write_export VIBEDOCK_DIRECT_PROTOCOL "${VIBEDOCK_DIRECT_PROTOCOL:-openai-completions}"
write_export VIBEDOCK_DIRECT_THINKING "${VIBEDOCK_DIRECT_THINKING:-}"
write_export VIBEDOCK_PERMISSION_POLICY "${VIBEDOCK_PERMISSION_POLICY:-ask}"
write_export VIBEDOCK_DIRECT_AUTO_APPROVE "${VIBEDOCK_DIRECT_AUTO_APPROVE:-false}"
write_export VIBEDOCK_DIRECT_API_KEY "${VIBEDOCK_DIRECT_API_KEY:-}"
IFS=',' read -r -a extra_keys <<<"${VIBEDOCK_EXTRA_ENV_KEYS:-}"
for key in "${extra_keys[@]}"; do
  [[ -n "$key" ]] || continue
  write_export "$key" "${!key-}"
done
chmod 600 "$env_file"

if ! tmux has-session -t "$tmux_name" 2>/dev/null; then
  runner="source '$env_file'; export NPM_CONFIG_PREFIX='$HOME/.local'; export PATH='$HOME/.local/bin:/opt/vibedock-cli/node_modules/.bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin'; /usr/local/bin/vibedock-agent-runner; exec /bin/bash -l"
  printf -v tmux_command 'bash -lc %q' "$runner"
  tmux new-session -d -s "$tmux_name" -c /workspace "$tmux_command"
  tmux set-option -t "$tmux_name" history-limit 100000 >/dev/null
fi

if [[ -f "$pid_file" ]]; then
  old_pid="$(cat "$pid_file" 2>/dev/null || true)"
  if [[ -n "$old_pid" ]] && kill -0 "$old_pid" 2>/dev/null; then
    exit 0
  fi
fi

nohup /usr/local/bin/ttyd \
  -p "$SESSION_PORT" \
  -W \
  -c "vibe:${WORKSPACE_PASSWORD}" \
  --base-path "$base_path" \
  -t titleFixed="VibeDock Session" \
  bash -lc "exec tmux attach-session -t '$tmux_name'" \
  >"$log_file" 2>&1 &
echo $! >"$pid_file"
sleep 0.15
kill -0 "$(cat "$pid_file")"
`

const stopSessionScript = `
set -Eeuo pipefail
state_dir="$HOME/.vibedock/sessions"
runtime_dir="/dev/shm/vibedock/sessions"
tmux_name="vd-${SESSION_ID}"
pid_file="$state_dir/${SESSION_ID}.pid"
if [[ -f "$pid_file" ]]; then
  pid="$(cat "$pid_file" 2>/dev/null || true)"
  if [[ -n "$pid" ]]; then
    kill "$pid" 2>/dev/null || true
  fi
  rm -f "$pid_file"
fi
rm -f "$runtime_dir/${SESSION_ID}.env"
tmux kill-session -t "$tmux_name" 2>/dev/null || true
`

const sessionStatusScript = `
set -Eeuo pipefail
pid_file="$HOME/.vibedock/sessions/${SESSION_ID}.pid"
tmux has-session -t "vd-${SESSION_ID}" 2>/dev/null
[[ -f "$pid_file" ]]
pid="$(cat "$pid_file")"
kill -0 "$pid" 2>/dev/null
`
