package docker

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"vibedock/internal/model"
)

const (
	testAuthAccount = "account12345"
	testAuthAttempt = "attempt12345678901234567"
	testCleanupID   = "cleanup1234567890123456789012345"
)

func writeAuthDocker(t *testing.T, script string) (string, string) {
	t.Helper()
	temporary := t.TempDir()
	path := filepath.Join(temporary, "docker")
	logPath := filepath.Join(temporary, "commands.log")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMMAND_LOG", logPath)
	return path, logPath
}

func protocolResponseScript(extra string) string {
	return `
printf '%s\n' "$*" >>"$COMMAND_LOG"
case "$*" in
  *" protocol") printf '%s\n' '{"protocol":1,"codex_version":"0.144.6","claude_version":"2.1.215","ttyd_version":"1.7.7"}'; exit 0 ;;
esac
` + extra
}

func readCommands(t *testing.T, path string) string {
	t.Helper()
	value, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(value)
}

func TestStartAuthUsesFixedHardenedContainer(t *testing.T) {
	expires := time.Now().UTC().Add(10 * time.Minute).Unix()
	docker, logPath := writeAuthDocker(t, protocolResponseScript(`
case "$1" in
  network) exit 0 ;;
  volume) printf '%s\n' 'vibedock-credentials'; exit 0 ;;
  create) printf '%s\n' 'container-id'; exit 0 ;;
  start) exit 0 ;;
  run)
    case "$*" in
      *" inspect "*) printf '%s\n' '{"protocol":1,"account_id":"account12345","attempt_id":"attempt12345678901234567","provider":"codex","state":"awaiting_user","verification_url":"https://auth.openai.com/codex/device","user_code":"CODE-123","expires_at":`+strconv.FormatInt(expires, 10)+`,"poll_after_ms":2000,"error_code":"","message":"waiting","terminal_available":false}'; exit 0 ;;
    esac ;;
esac
exit 1
`))
	runtime := New(docker, "vibedock")
	request := model.AuthStartRequest{AuthAttemptRef: model.AuthAttemptRef{AccountID: testAuthAccount, AttemptID: testAuthAttempt, Provider: model.AuthProviderCodex}, ExpiresAt: time.Unix(expires, 0).UTC()}
	attempt, err := runtime.StartAuth(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.State != model.AuthAttemptAwaitingUser || attempt.UserCode != "CODE-123" {
		t.Fatalf("unexpected attempt: %#v", attempt)
	}
	commands := readCommands(t, logPath)
	for _, expected := range []string{
		"--name vibedock-auth-" + testAuthAttempt,
		"--network vibedock",
		"--user 1001:1001",
		"--read-only",
		"--cap-drop ALL",
		"--security-opt no-new-privileges",
		"--pids-limit 128",
		"--cpus 1",
		"--memory 512m",
		"--restart no",
		"--log-driver none",
		"type=volume,src=vibedock-credentials,dst=/vibedock/credentials",
		"--entrypoint /usr/local/bin/vibedock-auth-helper",
		"vibedock/workspace:local login --provider codex",
		"--label vibedock.auth_helper=true",
		"--label vibedock.auth_account=" + testAuthAccount,
		"--label vibedock.auth_attempt=" + testAuthAttempt,
	} {
		if !strings.Contains(commands, expected) {
			t.Fatalf("missing fixed argument %q in:\n%s", expected, commands)
		}
	}
	for _, forbidden := range []string{"-p ", "--publish", "/var/run/docker.sock", "vibedock-ws-", "vibedock-home-", ":/workspace"} {
		if strings.Contains(commands, forbidden) {
			t.Fatalf("found forbidden argument %q in:\n%s", forbidden, commands)
		}
	}
}

func TestStartAuthErrorDoesNotExposeTerminalPasswordOrProviderOutput(t *testing.T) {
	docker, logPath := writeAuthDocker(t, protocolResponseScript(`
case "$1" in
  network|volume) exit 0 ;;
  create) printf '%s\n' 'provider-device-CODE-SECRET' >&2; exit 1 ;;
esac
exit 1
`))
	runtime := New(docker, "vibedock")
	request := model.AuthStartRequest{AuthAttemptRef: model.AuthAttemptRef{AccountID: testAuthAccount, AttemptID: testAuthAttempt, Provider: model.AuthProviderClaude}, ExpiresAt: time.Now().UTC().Add(10 * time.Minute)}
	_, err := runtime.StartAuth(context.Background(), request)
	if err == nil {
		t.Fatal("expected helper create failure")
	}
	if strings.Contains(err.Error(), "CODE-SECRET") {
		t.Fatalf("provider output leaked: %v", err)
	}
	commands := readCommands(t, logPath)
	if strings.Contains(commands, "VIBEDOCK_AUTH_TERMINAL_PASSWORD=") {
		t.Fatalf("terminal credential must not enter Docker container metadata: %s", commands)
	}
	if strings.Contains(err.Error(), "VIBEDOCK_AUTH_TERMINAL_PASSWORD") {
		t.Fatalf("terminal credential argument leaked through error: %v", err)
	}
}

func TestAuthStatusUsesFixedOfficialStatusHelper(t *testing.T) {
	docker, logPath := writeAuthDocker(t, protocolResponseScript(`
case "$1" in
  network|volume) exit 0 ;;
  run)
    case "$*" in
      *" status "*) printf '%s\n' '{"protocol":1,"status":"authenticated","checked_at":1700000000,"message":"checked"}'; exit 0 ;;
    esac ;;
esac
exit 1
`))
	runtime := New(docker, "vibedock")
	result, err := runtime.AuthStatus(context.Background(), model.AuthStatusRequest{AccountID: testAuthAccount, Provider: model.AuthProviderClaude})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != model.AuthStatusAuthenticated {
		t.Fatalf("unexpected status: %#v", result)
	}
	commands := readCommands(t, logPath)
	for _, expected := range []string{"status --provider claude --account " + testAuthAccount, "--label vibedock.auth_operation=status", "--label vibedock.auth_account=" + testAuthAccount, "--label vibedock.auth_provider=claude"} {
		if !strings.Contains(commands, expected) {
			t.Fatalf("missing fixed status argument %q: %s", expected, commands)
		}
	}
}

func TestAccountConfigWriteKeepsHelperStdinAttached(t *testing.T) {
	script := `
printf '%s\n' "$*" >>"$COMMAND_LOG"
case "$*" in
  *" config-write "*) printf '%s\n' '{"protocol":1,"path":"config.toml","exists":true,"content":"ok"}'; exit 0 ;;
esac
exit 1
`
	docker, logPath := writeAuthDocker(t, script)
	runtime := New(docker, "vibedock")
	document, err := runtime.WriteAccountConfig(context.Background(), testAuthAccount, model.AuthProviderCodex, "config.toml", "model = \"gpt-test\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if document.Content != "ok" {
		t.Fatalf("unexpected config document: %#v", document)
	}
	commands := readCommands(t, logPath)
	if !strings.Contains(commands, "run --rm -i") {
		t.Fatalf("config write did not attach stdin: %s", commands)
	}
}

func TestCredentialCleanupUsesOpaqueFixedOperations(t *testing.T) {
	docker, logPath := writeAuthDocker(t, protocolResponseScript(`
case "$1" in
  volume) exit 0 ;;
  run)
    case "$*" in
      *" cleanup-prepare "*) printf '%s\n' '{"protocol":1,"prepared":true,"completed":false}'; exit 0 ;;
      *" cleanup-commit "*|*" cleanup-rollback "*) printf '%s\n' '{"protocol":1,"prepared":false,"completed":true}'; exit 0 ;;
    esac ;;
esac
exit 1
`))
	runtime := New(docker, "vibedock")
	cleanup, err := runtime.PrepareAuthCredentialCleanup(context.Background(), testAuthAccount)
	if err != nil {
		t.Fatal(err)
	}
	if cleanup.AccountID != testAuthAccount || !authCleanupPattern.MatchString(cleanup.CleanupID) {
		t.Fatalf("unexpected cleanup reference: %#v", cleanup)
	}
	if err := runtime.CommitAuthCredentialCleanup(context.Background(), cleanup); err != nil {
		t.Fatal(err)
	}
	cleanup.CleanupID = testCleanupID
	if err := runtime.RollbackAuthCredentialCleanup(context.Background(), cleanup); err != nil {
		t.Fatal(err)
	}
	commands := readCommands(t, logPath)
	for _, expected := range []string{"cleanup-prepare --account " + testAuthAccount, "cleanup-commit --account " + testAuthAccount, "cleanup-rollback --account " + testAuthAccount} {
		if !strings.Contains(commands, expected) {
			t.Fatalf("missing cleanup operation %q in %s", expected, commands)
		}
	}
	if strings.Contains(commands, "rm -rf") || strings.Contains(commands, "volume rm") {
		t.Fatalf("unsafe cleanup command emitted: %s", commands)
	}
}

func TestReconcileOnlyRemovesExpiredLabeledAuthContainers(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	docker, logPath := writeAuthDocker(t, protocolResponseScript(`
case "$1" in
  volume) exit 0 ;;
  ps) printf '%s\n' 'aaaaaaaaaaaa'; exit 0 ;;
  inspect) printf '%s\n' '[{"State":{"Status":"running","Running":true},"Config":{"Labels":{"vibedock.managed":"true","vibedock.auth_helper":"true","vibedock.auth_account":"account12345","vibedock.auth_attempt":"attempt12345678901234567","vibedock.auth_provider":"codex","vibedock.auth_expires":"1700000000","vibedock.auth_protocol":"1"}},"NetworkSettings":{"Networks":{}}}]'; exit 0 ;;
  rm) exit 0 ;;
  run)
    case "$*" in
      *" reconcile "*) printf '%s\n' '{"protocol":1,"removed":1,"recovered":0}'; exit 0 ;;
    esac ;;
esac
exit 1
`))
	runtime := New(docker, "vibedock")
	if err := runtime.ReconcileAuth(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	commands := readCommands(t, logPath)
	if !strings.Contains(commands, "ps -aq --filter label=vibedock.auth_helper=true") || !strings.Contains(commands, "rm -f aaaaaaaaaaaa") {
		t.Fatalf("unexpected reconciliation commands: %s", commands)
	}
	for _, forbidden := range []string{"vibedock-ws-", "vibedock-home-", "volume rm"} {
		if strings.Contains(commands, forbidden) {
			t.Fatalf("reconciliation targeted forbidden object %q: %s", forbidden, commands)
		}
	}
}

func TestAuthTerminalRequiresMatchingLabelsAndMemorySecret(t *testing.T) {
	docker, _ := writeAuthDocker(t, `
printf '%s\n' "$*" >>"$COMMAND_LOG"
case "$1" in
  inspect) printf '%s\n' '[{"State":{"Status":"running","Running":true},"Config":{"Labels":{"vibedock.managed":"true","vibedock.auth_helper":"true","vibedock.auth_account":"account12345","vibedock.auth_attempt":"attempt12345678901234567","vibedock.auth_provider":"claude","vibedock.auth_protocol":"1"}},"NetworkSettings":{"Networks":{"vibedock":{"IPAddress":"172.30.0.8"}}}}]'; exit 0 ;;
esac
exit 1
`)
	ref := model.AuthAttemptRef{AccountID: testAuthAccount, AttemptID: testAuthAttempt, Provider: model.AuthProviderClaude}
	authTerminalSecrets.Store(authSecretKey(ref), "memory-only-password")
	defer authTerminalSecrets.Delete(authSecretKey(ref))
	runtime := New(docker, "vibedock")
	target, err := runtime.AuthTerminal(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if target.URL != "http://172.30.0.8:7681" || target.Username != "vibe" || target.Password != "memory-only-password" {
		t.Fatalf("unexpected terminal target: %#v", target)
	}
}
