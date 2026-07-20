package docker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vibedock/internal/model"
)

func TestSessionBootstrapScriptQuotesSecretsAndSettings(t *testing.T) {
	script := sessionBootstrapScript(model.SessionRuntimeRequest{
		WorkspaceID: "abcdef123456", SessionID: "123456abcdef", Agent: "codex", Port: 7700, Password: "p'ass",
		Account: model.AccountRuntime{ID: "account12345", AuthType: "api_key", BaseURL: "https://example.test/v1", Secret: "sk-'secret", ExtraEnv: map[string]string{"REGION": "jp"}},
	}, false)
	for _, expected := range []string{"SESSION_ID='123456abcdef'", "SESSION_PORT='7700'", "AGENT='codex'", "VIBEDOCK_BASE_URL='https://example.test/v1'", "REGION='jp'"} {
		if !strings.Contains(script, expected) {
			t.Fatalf("missing %q in bootstrap script", expected)
		}
	}
	if !strings.Contains(script, `'"'"'`) {
		t.Fatalf("single quotes were not safely escaped: %s", script)
	}
}

func TestRuntimeDefaults(t *testing.T) {
	runtime := New("", "")
	if runtime.Command != "docker" || runtime.Network != "vibedock" {
		t.Fatalf("unexpected defaults: %#v", runtime)
	}
}

func TestParseDockerStats(t *testing.T) {
	stats, err := parseDockerStats(`{"CPUPerc":"125.25%","MemPerc":"12.50%","MemUsage":"512MiB / 4GiB","PIDs":"24"}`)
	if err != nil {
		t.Fatal(err)
	}
	if stats.CPUPercent != 125.25 || stats.MemoryPercent != 12.5 || stats.MemoryBytes != 512<<20 || stats.MemoryLimit != 4<<30 || stats.PIDs != 24 {
		t.Fatalf("unexpected stats: %#v", stats)
	}
}

func TestParseDockerBytes(t *testing.T) {
	cases := map[string]uint64{"0B": 0, "1kB": 1000, "1.5MB": 1500000, "2KiB": 2048, "1.25GiB": uint64(1.25 * (1 << 30))}
	for input, expected := range cases {
		actual, err := parseDockerBytes(input)
		if err != nil || actual != expected {
			t.Fatalf("parseDockerBytes(%q) = %d, %v; want %d", input, actual, err, expected)
		}
	}
	for _, input := range []string{"", "-1MiB", "NaN", "1XB"} {
		if _, err := parseDockerBytes(input); err == nil {
			t.Fatalf("expected %q to fail", input)
		}
	}
}

func TestResourceStatsUsesReadOnlyDockerCommands(t *testing.T) {
	temp := t.TempDir()
	logPath := filepath.Join(temp, "commands.log")
	scriptPath := filepath.Join(temp, "docker")
	script := `#!/bin/sh
printf '%s\n' "$*" >>"$COMMAND_LOG"
if [ "$1" = "inspect" ]; then
  printf '%s\n' '[{"State":{"Status":"running","Running":true},"HostConfig":{"Memory":4294967296,"NanoCpus":2000000000},"NetworkSettings":{"Networks":{}}}]'
  exit 0
fi
if [ "$1" = "stats" ]; then
  printf '%s\n' '{"CPUPerc":"12.5%","MemPerc":"25%","MemUsage":"1GiB / 4GiB","PIDs":"18"}'
  exit 0
fi
exit 1
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMMAND_LOG", logPath)
	runtime := New(scriptPath, "vibedock")
	stats, err := runtime.ResourceStats(context.Background(), "workspace-container")
	if err != nil {
		t.Fatal(err)
	}
	if !stats.Live || stats.CPUPercent == nil || *stats.CPUPercent != 12.5 || stats.MemoryBytes == nil || *stats.MemoryBytes != 1<<30 {
		t.Fatalf("unexpected runtime stats: %#v", stats)
	}
	commands, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(commands)
	for _, expected := range []string{"inspect workspace-container", "stats --no-stream --format {{json .}} workspace-container"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("missing command %q in %s", expected, text)
		}
	}
	for _, forbidden := range []string{"network create", "network connect", "start workspace-container"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("stats used mutating command %q: %s", forbidden, text)
		}
	}
}

func TestResourceStatsFromInspectUsesEffectiveLimits(t *testing.T) {
	inspect := containerInspect{}
	inspect.State.Status = "running"
	inspect.State.Running = true
	inspect.HostConfig.Memory = 4 << 30
	inspect.HostConfig.NanoCPUs = 2500000000
	stats := resourceStatsFromInspect(inspect, time.Unix(1, 0).UTC())
	if stats.MemoryLimit == nil || *stats.MemoryLimit != 4<<30 || stats.CPULimit == nil || *stats.CPULimit != 2.5 {
		t.Fatalf("unexpected limits: %#v", stats)
	}
	inspect.HostConfig.NanoCPUs = 0
	inspect.HostConfig.CPUQuota = 150000
	inspect.HostConfig.CPUPeriod = 100000
	stats = resourceStatsFromInspect(inspect, time.Unix(1, 0).UTC())
	if stats.CPULimit == nil || *stats.CPULimit != 1.5 {
		t.Fatalf("unexpected quota fallback: %#v", stats)
	}
}

func TestSessionBootstrapUsesCoderLocalToolchainAndAbsoluteInfrastructureCommands(t *testing.T) {
	temp := t.TempDir()
	logPath := filepath.Join(temp, "commands.log")
	scriptPath := filepath.Join(temp, "docker")
	script := `#!/bin/sh
printf '%s\n' "$*" >"$COMMAND_LOG"
cat >/dev/null
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMMAND_LOG", logPath)
	runtime := New(scriptPath, "vibedock")
	if err := runtime.EnsureSession(context.Background(), "workspace-container", model.SessionRuntimeRequest{WorkspaceID: "abcdef123456", SessionID: "123456abcdef", Agent: "shell", Port: 7700, Password: "pw"}); err != nil {
		t.Fatal(err)
	}
	commands, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(commands)
	for _, expected := range []string{"-u coder", "HOME=/home/coder", "NPM_CONFIG_PREFIX=/home/coder/.local", "PATH=/home/coder/.local/bin:/opt/vibedock-cli/node_modules/.bin:", "workspace-container bash -s"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("missing %q in docker exec: %s", expected, text)
		}
	}
	bootstrap := sessionBootstrapScript(model.SessionRuntimeRequest{WorkspaceID: "abcdef123456", SessionID: "123456abcdef", Agent: "shell", Port: 7700, Password: "pw"}, false)
	for _, expected := range []string{"export NPM_CONFIG_PREFIX='$HOME/.local'", "export PATH='$HOME/.local/bin:/opt/vibedock-cli/node_modules/.bin:", "/usr/local/bin/vibedock-agent-runner", "/usr/local/bin/ttyd"} {
		if !strings.Contains(bootstrap, expected) {
			t.Fatalf("bootstrap did not pin infrastructure command %q", expected)
		}
	}
}

func TestSessionBootstrapScriptIncludesDirectAgentParameters(t *testing.T) {
	script := sessionBootstrapScript(model.SessionRuntimeRequest{
		WorkspaceID: "abcdef123456", SessionID: "123456abcdef", Agent: "opencode", Port: 7701, Password: "pw",
		Direct: model.DirectAgentRuntime{Provider: "relay", BaseURL: "https://relay.example/v1", Model: "gpt-test", Protocol: "openai-completions", AutoApprove: true, Secret: "sk-direct"},
	}, false)
	for _, expected := range []string{
		"AGENT='opencode'",
		"VIBEDOCK_DIRECT_PROVIDER='relay'",
		"VIBEDOCK_DIRECT_BASE_URL='https://relay.example/v1'",
		"VIBEDOCK_DIRECT_MODEL='gpt-test'",
		"VIBEDOCK_DIRECT_PROTOCOL='openai-completions'",
		"VIBEDOCK_DIRECT_AUTO_APPROVE='true'",
		"VIBEDOCK_DIRECT_API_KEY='sk-direct'",
	} {
		if !strings.Contains(script, expected) {
			t.Fatalf("missing %q in bootstrap script", expected)
		}
	}
}
