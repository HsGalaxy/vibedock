package app

import (
	"reflect"
	"strings"
	"testing"

	"vibedock/internal/model"
)

func TestCompileLaunchCommandPreservesManagedDefaults(t *testing.T) {
	tests := []struct {
		name       string
		agent      string
		account    model.AccountRuntime
		direct     model.DirectAgentRuntime
		permission string
		want       []string
	}{
		{name: "codex login", agent: "codex", account: model.AccountRuntime{AuthType: "login", Model: "gpt-test"}, want: []string{"--model", "gpt-test"}},
		{name: "codex managed provider", agent: "codex", account: model.AccountRuntime{AuthType: "api_key", Model: "gpt-test"}, want: []string{"--profile", "vibedock"}},
		{name: "claude", agent: "claude", account: model.AccountRuntime{Model: "sonnet"}, permission: "acceptEdits", want: []string{"--model", "sonnet", "--permission-mode", "acceptEdits"}},
		{name: "pi", agent: "pi", direct: model.DirectAgentRuntime{Provider: "relay", Model: "gpt-test", Thinking: "high", AutoApprove: true}, want: []string{"--session-dir", "$HOME/.vibedock/agents/abc123abc123/pi/sessions", "--provider", "relay", "--model", "gpt-test", "--thinking", "high", "--approve"}},
		{name: "opencode", agent: "opencode", direct: model.DirectAgentRuntime{Provider: "relay", Model: "gpt-test"}, want: []string{"--model", "relay/gpt-test", "/workspace"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			command, err := compileLaunchCommand(test.agent, nil, test.account, test.direct, "abc123abc123", test.permission, false)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(command.Arguments, test.want) {
				t.Fatalf("arguments = %#v, want %#v", command.Arguments, test.want)
			}
		})
	}
}

func TestCompileLaunchCommandAppliesStructuredAndAdvancedOptions(t *testing.T) {
	profile, err := normalizeLaunchProfile("codex", &model.AgentLaunchProfile{
		Model: "override", CodexApproval: "on-request", CodexSandbox: "workspace-write", CodexWebSearch: true,
		CodexConfigOverrides: []string{"features.example=true"}, ExtraArgs: []string{"--no-alt-screen"},
	})
	if err != nil {
		t.Fatal(err)
	}
	command, err := compileLaunchCommand("codex", profile, model.AccountRuntime{AuthType: "api_key", Model: "default"}, model.DirectAgentRuntime{}, "abc123abc123", "manual", false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--profile", "vibedock", "--model", "override", "--sandbox", "workspace-write", "--ask-for-approval", "on-request", "--search", "-c", "features.example=true", "--no-alt-screen"}
	if !reflect.DeepEqual(command.Arguments, want) {
		t.Fatalf("arguments = %#v, want %#v", command.Arguments, want)
	}
	if !strings.Contains(command.Preview, "features.example=true") {
		t.Fatalf("preview = %q", command.Preview)
	}
}

func TestNormalizeLaunchProfileRejectsControlCharactersAndUnsafeCustomSecrets(t *testing.T) {
	if _, err := normalizeLaunchProfile("claude", &model.AgentLaunchProfile{ExtraArgs: []string{"ok\n--bad"}}); err == nil {
		t.Fatal("expected control character rejection")
	}
	if _, err := normalizeLaunchProfile("pi", &model.AgentLaunchProfile{ConfigMode: "custom", CustomConfig: `{"providers":{"relay":{"apiKey":"plaintext"}}}`}); err == nil {
		t.Fatal("expected missing environment placeholder rejection")
	}
	profile, err := normalizeLaunchProfile("opencode", &model.AgentLaunchProfile{ConfigMode: "custom", CustomConfig: `{"provider":{"relay":{"options":{"apiKey":"{env:VIBEDOCK_DIRECT_API_KEY}"}}}}`})
	if err != nil || profile.ConfigMode != "custom" {
		t.Fatalf("expected valid custom profile, got %#v, %v", profile, err)
	}
}
