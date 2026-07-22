package docker

import (
	"context"
	"net/url"
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
  *"/vibedock/credentials/.vibedock-initialized") exit 0 ;;
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

func authorizationURLForTest(provider model.AuthProvider) string {
	values := url.Values{
		"response_type":         {"code"},
		"code_challenge_method": {"S256"},
		"state":                 {"abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQ"},
		"code_challenge":        {"abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQ"},
	}
	base := ""
	if provider == model.AuthProviderCodex {
		base = "https://auth.openai.com/oauth/authorize"
		values.Set("client_id", codexOAuthClientID)
		values.Set("redirect_uri", codexOAuthRedirectURI)
		values.Set("scope", codexOAuthScopes)
		values.Set("id_token_add_organizations", "true")
		values.Set("codex_cli_simplified_flow", "true")
		values.Set("originator", "codex_cli_rs")
	} else {
		base = "https://claude.com/cai/oauth/authorize"
		values.Set("client_id", claudeOAuthClientID)
		values.Set("redirect_uri", claudeOAuthRedirectURI)
		values.Set("scope", claudeOAuthScopes)
		values.Set("code", "true")
	}
	return base + "?" + values.Encode()
}

func TestValidateAuthorizationURLAllowsOnlyCanonicalProviderRoutes(t *testing.T) {
	for _, provider := range []model.AuthProvider{model.AuthProviderCodex, model.AuthProviderClaude} {
		if err := validateAuthorizationURL(provider, authorizationURLForTest(provider)); err != nil {
			t.Fatalf("valid %s URL rejected: %v", provider, err)
		}
	}
	codexURL := authorizationURLForTest(model.AuthProviderCodex)
	invalid := []string{
		strings.Replace(codexURL, "auth.openai.com", "login.example.com", 1),
		codexURL + "&state=duplicate",
		strings.Replace(codexURL, "scope=", "missing_scope=", 1),
	}
	for _, value := range invalid {
		if err := validateAuthorizationURL(model.AuthProviderCodex, value); err == nil {
			t.Fatalf("unsafe authorization URL accepted: %s", value)
		}
	}
	if err := validateAuthorizationURL(model.AuthProviderClaude, codexURL); err == nil {
		t.Fatal("provider-mismatched authorization URL accepted")
	}
}

func TestStartAuthUsesHardenedOneShotHelper(t *testing.T) {
	expires := time.Now().UTC().Add(10 * time.Minute).Unix()
	docker, logPath := writeAuthDocker(t, protocolResponseScript(`
case "$1" in
  volume) printf '%s\n' 'vibedock-credentials'; exit 0 ;;
  run)
    case "$*" in
      *" login "*) printf '%s\n' '{"protocol":1,"account_id":"account12345","attempt_id":"attempt12345678901234567","provider":"codex","state":"awaiting_user","verification_url":"https://auth.openai.com/oauth/authorize?client_id=app_EMoamEEZ73f0CkXaXp7hrann&redirect_uri=http%3A%2F%2Flocalhost%3A1455%2Fauth%2Fcallback&response_type=code&scope=openid+profile+email+offline_access+api.connectors.read+api.connectors.invoke&code_challenge_method=S256&state=abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQ&code_challenge=abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQ&id_token_add_organizations=true&codex_cli_simplified_flow=true&originator=codex_cli_rs","user_code":"","expires_at":`+strconv.FormatInt(expires, 10)+`,"poll_after_ms":2000,"error_code":"","message":"waiting","terminal_available":false}'; exit 0 ;;
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
	if attempt.State != model.AuthAttemptAwaitingUser || !strings.HasPrefix(attempt.VerificationURL, "https://auth.openai.com/oauth/authorize?") {
		t.Fatalf("unexpected attempt: %#v", attempt)
	}
	commands := readCommands(t, logPath)
	for _, expected := range []string{
		"run --rm --network none --user 0:0 --read-only --cap-drop ALL --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add FOWNER",
		"--pids-limit 32 --cpus 0.25 --memory 64m --restart no --log-driver none",
		"--entrypoint /bin/sh vibedock/workspace:local -ec if [ ! -L /vibedock/credentials/.vibedock-initialized ] && [ -f /vibedock/credentials/.vibedock-initialized ]",
		"$(/usr/bin/stat -c %u:%g:%a /vibedock/credentials)",
		"temporary=$(/usr/bin/mktemp /vibedock/credentials/.vibedock-initializing.XXXXXX)",
		"/usr/bin/mv -f \"$temporary\" /vibedock/credentials/.vibedock-initialized",
	} {
		if !strings.Contains(commands, expected) {
			t.Fatalf("credential volume initializer missing %q:\n%s", expected, commands)
		}
	}
	initializationArgs := strings.Join(authVolumeInitializationArgs(), " ")
	if strings.Contains(initializationArgs, "chown 0:0") || strings.Index(initializationArgs, "chmod 0700 /vibedock/credentials") > strings.Index(initializationArgs, "mktemp /vibedock/credentials/.vibedock-initializing") || strings.Index(initializationArgs, "mktemp /vibedock/credentials/.vibedock-initializing") > strings.Index(initializationArgs, "mv -f") {
		t.Fatalf("credential initializer cannot recover safely from a partial run: %s", initializationArgs)
	}
	for _, expected := range []string{
		"run --rm --network none",
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
		"--account " + testAuthAccount,
		"--attempt " + testAuthAttempt,
	} {
		if !strings.Contains(commands, expected) {
			t.Fatalf("missing fixed argument %q in:\n%s", expected, commands)
		}
	}
	for _, forbidden := range []string{"--privileged", "--name vibedock-auth-", " start ", " exec ", "--label vibedock.auth_", "-p ", "--publish", "/var/run/docker.sock", "vibedock-ws-", "vibedock-home-", ":/workspace"} {
		if strings.Contains(commands, forbidden) {
			t.Fatalf("found forbidden argument %q in:\n%s", forbidden, commands)
		}
	}
}

func TestStartAuthErrorDoesNotExposeProviderOutput(t *testing.T) {
	docker, logPath := writeAuthDocker(t, protocolResponseScript(`
case "$1" in
  volume) exit 0 ;;
  run)
    case "$*" in
      *" login "*) printf '%s\n' 'provider-authorization-CODE-SECRET' >&2; exit 1 ;;
    esac ;;
esac
exit 1
`))
	runtime := New(docker, "vibedock")
	request := model.AuthStartRequest{AuthAttemptRef: model.AuthAttemptRef{AccountID: testAuthAccount, AttemptID: testAuthAttempt, Provider: model.AuthProviderClaude}, ExpiresAt: time.Now().UTC().Add(10 * time.Minute)}
	_, err := runtime.StartAuth(context.Background(), request)
	if err == nil {
		t.Fatal("expected helper login failure")
	}
	if strings.Contains(err.Error(), "CODE-SECRET") {
		t.Fatalf("provider output leaked: %v", err)
	}
	commands := readCommands(t, logPath)
	if strings.Contains(commands, "CODE-SECRET") {
		t.Fatalf("provider output entered Docker command metadata: %s", commands)
	}
}

func TestAuthStatusUsesOfflineCredentialInspection(t *testing.T) {
	docker, logPath := writeAuthDocker(t, protocolResponseScript(`
case "$1" in
  volume) exit 0 ;;
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
	for _, expected := range []string{"status --provider claude --account " + testAuthAccount, "run --rm --network none"} {
		if !strings.Contains(commands, expected) {
			t.Fatalf("missing fixed status argument %q: %s", expected, commands)
		}
	}
	if strings.Contains(commands, "network inspect") || strings.Contains(commands, "--label vibedock.auth_operation=status") {
		t.Fatalf("status unexpectedly required network or a long-running helper: %s", commands)
	}
}

func TestCompleteAuthPassesCallbackOnlyOnStdin(t *testing.T) {
	expires := time.Now().UTC().Add(10 * time.Minute).Unix()
	docker, logPath := writeAuthDocker(t, protocolResponseScript(`
case "$1" in
  network|volume) exit 0 ;;
  run)
    case "$*" in
      *" complete "*) printf '%s\n' '{"protocol":1,"account_id":"account12345","attempt_id":"attempt12345678901234567","provider":"codex","state":"completed","verification_url":"","user_code":"","expires_at":`+strconv.FormatInt(expires, 10)+`,"poll_after_ms":2000,"error_code":"","message":"completed","terminal_available":false}'; exit 0 ;;
    esac ;;
esac
exit 1
`))
	runtime := New(docker, "vibedock")
	callback := "http://localhost:1455/auth/callback?code=provider-secret&state=oauth-state"
	request := model.AuthCompleteRequest{
		AuthAttemptRef:        model.AuthAttemptRef{AccountID: testAuthAccount, AttemptID: testAuthAttempt, Provider: model.AuthProviderCodex},
		AuthorizationResponse: callback,
	}
	attempt, err := runtime.CompleteAuth(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.State != model.AuthAttemptCompleted {
		t.Fatalf("unexpected attempt: %#v", attempt)
	}
	commands := readCommands(t, logPath)
	if !strings.Contains(commands, "run --rm -i --network vibedock") || !strings.Contains(commands, "complete --provider codex --account "+testAuthAccount+" --attempt "+testAuthAttempt) {
		t.Fatalf("unexpected completion command: %s", commands)
	}
	if strings.Contains(commands, "provider-secret") || strings.Contains(commands, "oauth-state") {
		t.Fatalf("authorization response leaked into Docker arguments: %s", commands)
	}
}

func TestCancelAuthUsesOneShotHelper(t *testing.T) {
	expires := time.Now().UTC().Add(10 * time.Minute).Unix()
	docker, logPath := writeAuthDocker(t, protocolResponseScript(`
case "$1" in
  volume) exit 0 ;;
  run)
    case "$*" in
      *" cancel "*) printf '%s\n' '{"protocol":1,"account_id":"account12345","attempt_id":"attempt12345678901234567","provider":"claude","state":"cancelled","verification_url":"","user_code":"","expires_at":`+strconv.FormatInt(expires, 10)+`,"poll_after_ms":2000,"error_code":"","message":"cancelled","terminal_available":false}'; exit 0 ;;
    esac ;;
esac
exit 1
`))
	runtime := New(docker, "vibedock")
	ref := model.AuthAttemptRef{AccountID: testAuthAccount, AttemptID: testAuthAttempt, Provider: model.AuthProviderClaude}
	if err := runtime.CancelAuth(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	commands := readCommands(t, logPath)
	if !strings.Contains(commands, "cancel --provider claude --account "+testAuthAccount+" --attempt "+testAuthAttempt) || strings.Contains(commands, "stop --time") || strings.Contains(commands, "rm -f vibedock-auth-") {
		t.Fatalf("cancel did not use one-shot helper: %s", commands)
	}
}

func TestAccountConfigWriteKeepsHelperStdinAttached(t *testing.T) {
	script := protocolResponseScript(`
case "$1" in
  volume) printf '%s\n' 'vibedock-credentials'; exit 0 ;;
esac
case "$*" in
  *" config-write "*) printf '%s\n' '{"protocol":1,"path":"config.toml","exists":true,"content":"ok"}'; exit 0 ;;
esac
exit 1
`)
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
	if !strings.Contains(commands, "volume create vibedock-credentials") || !strings.Contains(commands, "--entrypoint /bin/sh vibedock/workspace:local -ec if [ ! -L /vibedock/credentials/.vibedock-initialized ]") || !strings.Contains(commands, ".vibedock-initialized") {
		t.Fatalf("config write did not initialize fresh credential storage: %s", commands)
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
