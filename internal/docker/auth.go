package docker

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"vibedock/internal/model"
)

const (
	authHelperDefaultImage = "vibedock/workspace:local"
	authHelperEntrypoint   = "/usr/local/bin/vibedock-auth-helper"
	authHelperProtocol     = 1
	authCredentialsVolume  = "vibedock-credentials"
	authOutputLimit        = 1 << 20
	authResponseLimit      = 8 << 10
	codexOAuthClientID     = "app_EMoamEEZ73f0CkXaXp7hrann"
	codexOAuthRedirectURI  = "http://localhost:1455/auth/callback"
	codexOAuthScopes       = "openid profile email offline_access api.connectors.read api.connectors.invoke"
	claudeOAuthClientID    = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	claudeOAuthRedirectURI = "https://platform.claude.com/oauth/code/callback"
	claudeOAuthScopes      = "org:create_api_key user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"
)

var (
	authAccountPattern = regexp.MustCompile(`^[a-z0-9]{12}$`)
	authAttemptPattern = regexp.MustCompile(`^[a-z0-9]{24}$`)
	authCleanupPattern = regexp.MustCompile(`^[a-z0-9]{32}$`)
	authOAuthPattern   = regexp.MustCompile(`^[A-Za-z0-9._~-]{20,256}$`)
)

type authHelperState struct {
	Protocol          int    `json:"protocol"`
	AccountID         string `json:"account_id"`
	AttemptID         string `json:"attempt_id"`
	Provider          string `json:"provider"`
	State             string `json:"state"`
	VerificationURL   string `json:"verification_url"`
	UserCode          string `json:"user_code"`
	ExpiresAt         int64  `json:"expires_at"`
	PollAfterMS       int64  `json:"poll_after_ms"`
	ErrorCode         string `json:"error_code"`
	Message           string `json:"message"`
	TerminalAvailable bool   `json:"terminal_available"`
}

type authHelperManifest struct {
	Protocol      int    `json:"protocol"`
	CodexVersion  string `json:"codex_version"`
	ClaudeVersion string `json:"claude_version"`
	TTydVersion   string `json:"ttyd_version"`
}

type authHelperStatus struct {
	Protocol  int    `json:"protocol"`
	Status    string `json:"status"`
	CheckedAt int64  `json:"checked_at"`
	Message   string `json:"message"`
}

type authHelperLogout struct {
	Protocol                int    `json:"protocol"`
	LocalCredentialsRemoved bool   `json:"local_credentials_removed"`
	ProviderLogoutConfirmed bool   `json:"provider_logout_confirmed"`
	Message                 string `json:"message"`
}

type authHelperAck struct {
	Protocol  int  `json:"protocol"`
	Prepared  bool `json:"prepared"`
	Completed bool `json:"completed"`
}

type authContainerInspect struct {
	State struct {
		Status  string `json:"Status"`
		Running bool   `json:"Running"`
	} `json:"State"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

type cappedWriter struct {
	buffer   bytes.Buffer
	limit    int
	exceeded bool
}

func (writer *cappedWriter) Write(value []byte) (int, error) {
	original := len(value)
	remaining := writer.limit - writer.buffer.Len()
	if remaining > 0 {
		if len(value) > remaining {
			value = value[:remaining]
		}
		_, _ = writer.buffer.Write(value)
	}
	if original > remaining {
		writer.exceeded = true
	}
	return original, nil
}

func (writer *cappedWriter) String() string {
	return writer.buffer.String()
}

func authHelperImage() string {
	if image := strings.TrimSpace(os.Getenv("VIBEDOCK_AUTH_HELPER_IMAGE")); image != "" {
		return image
	}
	return authHelperDefaultImage
}

func validateAuthProvider(provider model.AuthProvider) error {
	if provider != model.AuthProviderCodex && provider != model.AuthProviderClaude {
		return errors.New("unsupported authentication provider")
	}
	return nil
}

func validateAuthRef(ref model.AuthAttemptRef) error {
	if !authAccountPattern.MatchString(ref.AccountID) {
		return errors.New("invalid authentication account")
	}
	if !authAttemptPattern.MatchString(ref.AttemptID) {
		return errors.New("invalid authentication attempt")
	}
	return validateAuthProvider(ref.Provider)
}

func validateAuthAccount(accountID string) error {
	if !authAccountPattern.MatchString(accountID) {
		return errors.New("invalid authentication account")
	}
	return nil
}

func authCleanupID() (string, error) {
	value := make([]byte, 20)
	if _, err := rand.Read(value); err != nil {
		return "", errors.New("generate credential cleanup identifier")
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(value)), nil
}

func authBaseDockerArgs(network string) []string {
	return []string{
		"--network", network,
		"--user", "1001:1001",
		"--read-only",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--pids-limit", "128",
		"--cpus", "1",
		"--memory", "512m",
		"--tmpfs", "/home/coder:rw,noexec,nosuid,nodev,size=64m,uid=1001,gid=1001,mode=0700",
		"--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=64m,uid=1001,gid=1001,mode=0700",
		"--restart", "no",
		"--log-driver", "none",
		"--mount", "type=volume,src=" + authCredentialsVolume + ",dst=/vibedock/credentials",
	}
}

func authOneShotDockerArgs(network string, networkEnabled bool) []string {
	if !networkEnabled {
		network = "none"
	}
	return authBaseDockerArgs(network)
}

func (r *Runtime) authPrerequisites(ctx context.Context, network bool) error {
	if network {
		if err := r.ensureNetwork(ctx); err != nil {
			return errors.New("authentication helper network unavailable")
		}
	}
	if err := r.authEnsureStorage(ctx); err != nil {
		return err
	}
	return r.authProtocol(ctx)
}

func (r *Runtime) authEnsureStorage(ctx context.Context) error {
	if _, err := r.authRun(ctx, "create credentials volume", "volume", "create", authCredentialsVolume); err != nil {
		return err
	}
	if _, err := r.authRun(ctx, "initialize credentials volume", authVolumeInitializationArgs()...); err != nil {
		return err
	}
	return nil
}

func authVolumeInitializationArgs() []string {
	return []string{
		"run", "--rm",
		"--network", "none",
		"--user", "0:0",
		"--read-only",
		"--cap-drop", "ALL",
		"--cap-add", "CHOWN",
		"--cap-add", "DAC_OVERRIDE",
		"--cap-add", "FOWNER",
		"--security-opt", "no-new-privileges",
		"--pids-limit", "32",
		"--cpus", "0.25",
		"--memory", "64m",
		"--restart", "no",
		"--log-driver", "none",
		"--mount", "type=volume,src=" + authCredentialsVolume + ",dst=/vibedock/credentials",
		"--entrypoint", "/bin/sh",
		authHelperImage(),
		"-ec", "if [ ! -L /vibedock/credentials/.vibedock-initialized ] && [ -f /vibedock/credentials/.vibedock-initialized ] && [ \"$(/usr/bin/stat -c %u:%g:%a /vibedock/credentials)\" = 1001:1001:700 ] && [ \"$(/usr/bin/stat -c %u:%g:%a /vibedock/credentials/.vibedock-initialized)\" = 1001:1001:600 ]; then exit 0; fi; /usr/bin/chmod 0700 /vibedock/credentials && /usr/bin/chown 1001:1001 /vibedock/credentials && temporary=$(/usr/bin/mktemp /vibedock/credentials/.vibedock-initializing.XXXXXX) && /usr/bin/chmod 0600 \"$temporary\" && /usr/bin/chown 1001:1001 \"$temporary\" && /usr/bin/mv -f \"$temporary\" /vibedock/credentials/.vibedock-initialized",
	}
}

func (r *Runtime) authProtocol(ctx context.Context) error {
	var manifest authHelperManifest
	if err := r.authOneShotJSON(ctx, false, "protocol", nil, &manifest); err != nil {
		return err
	}
	if manifest.Protocol != authHelperProtocol {
		return errors.New("authentication helper protocol incompatible")
	}
	return nil
}

func (r *Runtime) authOneShotJSON(ctx context.Context, network bool, operation string, operationArgs []string, result any) error {
	return r.authOneShotJSONInput(ctx, network, operation, operationArgs, "", result)
}

func (r *Runtime) authOneShotJSONInput(ctx context.Context, network bool, operation string, operationArgs []string, input string, result any) error {
	args := []string{"run", "--rm"}
	if input != "" {
		args = append(args, "-i")
	}
	args = append(args, authOneShotDockerArgs(r.Network, network)...)
	args = append(args, "--entrypoint", authHelperEntrypoint, authHelperImage(), operation)
	args = append(args, operationArgs...)
	output, err := r.authRunWithInput(ctx, operation, input, args...)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(output))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(result); err != nil {
		return errors.New("authentication helper returned an incompatible response")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("authentication helper returned an incompatible response")
	}
	return nil
}

func validateAccountConfigPath(provider model.AuthProvider, path string) error {
	if path == "" || strings.Contains(path, "/") || strings.Contains(path, "\\") || path == "." || path == ".." {
		return errors.New("invalid account configuration path")
	}
	switch provider {
	case model.AuthProviderCodex:
		if path != "config.toml" {
			return errors.New("configuration path is not allowlisted")
		}
	case model.AuthProviderClaude:
		if path != "settings.json" {
			return errors.New("configuration path is not allowlisted")
		}
	default:
		return errors.New("unsupported configuration provider")
	}
	return nil
}

func (r *Runtime) ListAccountConfig(ctx model.Context, accountID string, provider model.AuthProvider) ([]model.AccountConfigFile, error) {
	if err := validateAuthAccount(accountID); err != nil {
		return nil, err
	}
	if err := validateAuthProvider(provider); err != nil {
		return nil, err
	}
	var response struct {
		Protocol int                       `json:"protocol"`
		Files    []model.AccountConfigFile `json:"files"`
	}
	cctx, cancel := context.WithTimeout(asContext(ctx), 20*time.Second)
	defer cancel()
	if err := r.authEnsureStorage(cctx); err != nil {
		return nil, err
	}
	if err := r.authOneShotJSON(cctx, false, "config-list", []string{"--account", accountID, "--provider", string(provider)}, &response); err != nil {
		return nil, err
	}
	if response.Protocol != authHelperProtocol {
		return nil, errors.New("configuration helper protocol incompatible")
	}
	return response.Files, nil
}

func (r *Runtime) ReadAccountConfig(ctx model.Context, accountID string, provider model.AuthProvider, path string) (model.AccountConfigDocument, error) {
	if err := validateAuthAccount(accountID); err != nil {
		return model.AccountConfigDocument{}, err
	}
	if err := validateAuthProvider(provider); err != nil {
		return model.AccountConfigDocument{}, err
	}
	if err := validateAccountConfigPath(provider, path); err != nil {
		return model.AccountConfigDocument{}, err
	}
	var response struct {
		Protocol int    `json:"protocol"`
		Path     string `json:"path"`
		Exists   bool   `json:"exists"`
		Content  string `json:"content"`
	}
	cctx, cancel := context.WithTimeout(asContext(ctx), 20*time.Second)
	defer cancel()
	if err := r.authEnsureStorage(cctx); err != nil {
		return model.AccountConfigDocument{}, err
	}
	if err := r.authOneShotJSON(cctx, false, "config-read", []string{"--account", accountID, "--provider", string(provider), "--path", path}, &response); err != nil {
		return model.AccountConfigDocument{}, err
	}
	if response.Protocol != authHelperProtocol || response.Path != path {
		return model.AccountConfigDocument{}, errors.New("configuration helper protocol incompatible")
	}
	return model.AccountConfigDocument{Path: response.Path, Exists: response.Exists, Content: response.Content}, nil
}

func (r *Runtime) WriteAccountConfig(ctx model.Context, accountID string, provider model.AuthProvider, path, content string) (model.AccountConfigDocument, error) {
	if err := validateAuthAccount(accountID); err != nil {
		return model.AccountConfigDocument{}, err
	}
	if err := validateAuthProvider(provider); err != nil {
		return model.AccountConfigDocument{}, err
	}
	if err := validateAccountConfigPath(provider, path); err != nil {
		return model.AccountConfigDocument{}, err
	}
	if len(content) == 0 || len(content) > 256<<10 {
		return model.AccountConfigDocument{}, errors.New("configuration content is empty or too large")
	}
	var response struct {
		Protocol int    `json:"protocol"`
		Path     string `json:"path"`
		Exists   bool   `json:"exists"`
		Content  string `json:"content"`
	}
	cctx, cancel := context.WithTimeout(asContext(ctx), 20*time.Second)
	defer cancel()
	if err := r.authEnsureStorage(cctx); err != nil {
		return model.AccountConfigDocument{}, err
	}
	if err := r.authOneShotJSONInput(cctx, false, "config-write", []string{"--account", accountID, "--provider", string(provider), "--path", path}, content, &response); err != nil {
		return model.AccountConfigDocument{}, err
	}
	if response.Protocol != authHelperProtocol || response.Path != path {
		return model.AccountConfigDocument{}, errors.New("configuration helper protocol incompatible")
	}
	return model.AccountConfigDocument{Path: response.Path, Exists: response.Exists, Content: response.Content}, nil
}

func (r *Runtime) ResetAccountConfig(ctx model.Context, accountID string, provider model.AuthProvider, path string) error {
	if err := validateAuthAccount(accountID); err != nil {
		return err
	}
	if err := validateAuthProvider(provider); err != nil {
		return err
	}
	if err := validateAccountConfigPath(provider, path); err != nil {
		return err
	}
	var response struct {
		Protocol int  `json:"protocol"`
		Removed  bool `json:"removed"`
	}
	cctx, cancel := context.WithTimeout(asContext(ctx), 20*time.Second)
	defer cancel()
	if err := r.authEnsureStorage(cctx); err != nil {
		return err
	}
	if err := r.authOneShotJSON(cctx, false, "config-reset", []string{"--account", accountID, "--provider", string(provider), "--path", path}, &response); err != nil {
		return err
	}
	if response.Protocol != authHelperProtocol {
		return errors.New("configuration helper protocol incompatible")
	}
	return nil
}

func (r *Runtime) authRun(ctx context.Context, operation string, args ...string) (string, error) {
	return r.authRunWithInput(ctx, operation, "", args...)
}

func (r *Runtime) authRunWithInput(ctx context.Context, operation, input string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, r.Command, args...)
	stdout := &cappedWriter{limit: authOutputLimit}
	command.Stdout = stdout
	command.Stderr = io.Discard
	if input != "" {
		command.Stdin = strings.NewReader(input)
	}
	if err := command.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("authentication helper %s timed out", operation)
		}
		return "", fmt.Errorf("authentication helper %s failed", operation)
	}
	if stdout.exceeded {
		return "", errors.New("authentication helper response exceeded limit")
	}
	return strings.TrimSpace(stdout.String()), nil
}

func (r *Runtime) StartAuth(ctx model.Context, request model.AuthStartRequest) (model.AuthRuntimeAttempt, error) {
	if err := validateAuthRef(request.AuthAttemptRef); err != nil {
		return model.AuthRuntimeAttempt{}, err
	}
	now := time.Now().UTC()
	if !request.ExpiresAt.After(now) || request.ExpiresAt.After(now.Add(16*time.Minute)) {
		return model.AuthRuntimeAttempt{}, errors.New("invalid authentication expiry")
	}
	cctx, cancel := context.WithTimeout(asContext(ctx), 45*time.Second)
	defer cancel()
	if err := r.authPrerequisites(cctx, false); err != nil {
		return model.AuthRuntimeAttempt{}, err
	}
	ref := request.AuthAttemptRef
	var state authHelperState
	if err := r.authOneShotJSON(cctx, false, "login", []string{
		"--provider", string(ref.Provider),
		"--account", ref.AccountID,
		"--attempt", ref.AttemptID,
		"--expires-at", strconv.FormatInt(request.ExpiresAt.Unix(), 10),
	}, &state); err != nil {
		return model.AuthRuntimeAttempt{}, err
	}
	attempt, err := validateAuthHelperState(ref, state)
	if err != nil {
		return model.AuthRuntimeAttempt{}, err
	}
	if attempt.State != model.AuthAttemptAwaitingUser {
		return model.AuthRuntimeAttempt{}, errors.New("authentication helper did not start authorization")
	}
	return attempt, nil
}

func (r *Runtime) CompleteAuth(ctx model.Context, request model.AuthCompleteRequest) (model.AuthRuntimeAttempt, error) {
	if err := validateAuthRef(request.AuthAttemptRef); err != nil {
		return model.AuthRuntimeAttempt{}, err
	}
	if request.AuthorizationResponse == "" || len(request.AuthorizationResponse) > authResponseLimit || strings.IndexByte(request.AuthorizationResponse, 0) >= 0 {
		return model.AuthRuntimeAttempt{}, errors.New("invalid authorization response")
	}
	cctx, cancel := context.WithTimeout(asContext(ctx), 75*time.Second)
	defer cancel()
	if err := r.authPrerequisites(cctx, true); err != nil {
		return model.AuthRuntimeAttempt{}, err
	}
	ref := request.AuthAttemptRef
	var state authHelperState
	if err := r.authOneShotJSONInput(cctx, true, "complete", []string{
		"--provider", string(ref.Provider),
		"--account", ref.AccountID,
		"--attempt", ref.AttemptID,
	}, request.AuthorizationResponse, &state); err != nil {
		return model.AuthRuntimeAttempt{}, err
	}
	attempt, err := validateAuthHelperState(ref, state)
	if err != nil {
		return model.AuthRuntimeAttempt{}, err
	}
	if !attempt.State.Terminal() {
		return model.AuthRuntimeAttempt{}, errors.New("authentication helper did not finish authorization")
	}
	return attempt, nil
}

func (r *Runtime) InspectAuth(ctx model.Context, ref model.AuthAttemptRef) (model.AuthRuntimeAttempt, error) {
	if err := validateAuthRef(ref); err != nil {
		return model.AuthRuntimeAttempt{}, err
	}
	cctx, cancel := context.WithTimeout(asContext(ctx), 10*time.Second)
	defer cancel()
	return r.inspectAuth(cctx, ref)
}

func (r *Runtime) inspectAuth(ctx context.Context, ref model.AuthAttemptRef) (model.AuthRuntimeAttempt, error) {
	var state authHelperState
	if err := r.authOneShotJSON(ctx, false, "inspect", []string{
		"--provider", string(ref.Provider), "--account", ref.AccountID, "--attempt", ref.AttemptID,
	}, &state); err != nil {
		return model.AuthRuntimeAttempt{}, err
	}
	return validateAuthHelperState(ref, state)
}

func validateAuthHelperState(ref model.AuthAttemptRef, state authHelperState) (model.AuthRuntimeAttempt, error) {
	if state.Protocol != authHelperProtocol || state.AccountID != ref.AccountID || state.AttemptID != ref.AttemptID || state.Provider != string(ref.Provider) {
		return model.AuthRuntimeAttempt{}, errors.New("authentication helper returned mismatched state")
	}
	attemptState := model.AuthAttemptState(state.State)
	switch attemptState {
	case model.AuthAttemptStarting, model.AuthAttemptAwaitingUser, model.AuthAttemptVerifying, model.AuthAttemptCompleted, model.AuthAttemptCancelling, model.AuthAttemptCancelled, model.AuthAttemptExpired, model.AuthAttemptFailed:
	default:
		return model.AuthRuntimeAttempt{}, errors.New("authentication helper returned invalid state")
	}
	if state.PollAfterMS < 1000 || state.PollAfterMS > 30000 || state.ExpiresAt <= 0 {
		return model.AuthRuntimeAttempt{}, errors.New("authentication helper returned invalid timing")
	}
	if state.UserCode != "" || state.TerminalAvailable {
		return model.AuthRuntimeAttempt{}, errors.New("authentication helper returned obsolete login data")
	}
	if attemptState == model.AuthAttemptAwaitingUser {
		if err := validateAuthorizationURL(ref.Provider, state.VerificationURL); err != nil {
			return model.AuthRuntimeAttempt{}, errors.New("authentication helper returned unsafe authorization data")
		}
	} else if state.VerificationURL != "" {
		return model.AuthRuntimeAttempt{}, errors.New("authentication helper retained transient login data")
	}
	return model.AuthRuntimeAttempt{
		AuthAttemptRef:    ref,
		State:             attemptState,
		VerificationURL:   state.VerificationURL,
		UserCode:          state.UserCode,
		ExpiresAt:         time.Unix(state.ExpiresAt, 0).UTC(),
		PollAfter:         time.Duration(state.PollAfterMS) * time.Millisecond,
		ErrorCode:         state.ErrorCode,
		Message:           state.Message,
		TerminalAvailable: state.TerminalAvailable,
	}, nil
}

func validateAuthorizationURL(provider model.AuthProvider, value string) error {
	if len(value) < 32 || len(value) > 4096 {
		return errors.New("invalid verification URL")
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Fragment != "" || parsed.User != nil || parsed.RawPath != "" {
		return errors.New("invalid verification URL")
	}
	if parsed.Port() != "" && parsed.Port() != "443" {
		return errors.New("invalid verification URL")
	}
	expectedHost, expectedPath, expectedClient, expectedRedirect, expectedScopes := "", "", "", "", ""
	providerParameters := map[string]string{}
	switch provider {
	case model.AuthProviderCodex:
		expectedHost = "auth.openai.com"
		expectedPath = "/oauth/authorize"
		expectedClient = codexOAuthClientID
		expectedRedirect = codexOAuthRedirectURI
		expectedScopes = codexOAuthScopes
		providerParameters["id_token_add_organizations"] = "true"
		providerParameters["codex_cli_simplified_flow"] = "true"
		providerParameters["originator"] = "codex_cli_rs"
	case model.AuthProviderClaude:
		expectedHost = "claude.com"
		expectedPath = "/cai/oauth/authorize"
		expectedClient = claudeOAuthClientID
		expectedRedirect = claudeOAuthRedirectURI
		expectedScopes = claudeOAuthScopes
		providerParameters["code"] = "true"
	default:
		return errors.New("invalid verification URL")
	}
	if !strings.EqualFold(parsed.Hostname(), expectedHost) || parsed.Path != expectedPath {
		return errors.New("invalid verification URL")
	}
	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil {
		return errors.New("invalid verification URL")
	}
	required := map[string]string{
		"client_id":             expectedClient,
		"redirect_uri":          expectedRedirect,
		"response_type":         "code",
		"code_challenge_method": "S256",
		"scope":                 expectedScopes,
	}
	for key, value := range providerParameters {
		required[key] = value
	}
	for key, expected := range required {
		values, ok := query[key]
		if !ok || len(values) != 1 || values[0] != expected {
			return errors.New("invalid verification URL")
		}
	}
	state := query["state"]
	challenge := query["code_challenge"]
	if len(state) != 1 || !authOAuthPattern.MatchString(state[0]) || len(challenge) != 1 || len(challenge[0]) != 43 || !authOAuthPattern.MatchString(challenge[0]) {
		return errors.New("invalid verification URL")
	}
	return nil
}

func (r *Runtime) CancelAuth(ctx model.Context, ref model.AuthAttemptRef) error {
	if err := validateAuthRef(ref); err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(asContext(ctx), 30*time.Second)
	defer cancel()
	if err := r.authPrerequisites(cctx, false); err != nil {
		return err
	}
	var state authHelperState
	if err := r.authOneShotJSON(cctx, false, "cancel", []string{
		"--provider", string(ref.Provider), "--account", ref.AccountID, "--attempt", ref.AttemptID,
	}, &state); err != nil {
		return err
	}
	attempt, err := validateAuthHelperState(ref, state)
	if err != nil {
		return err
	}
	if !attempt.State.Terminal() {
		return errors.New("authentication helper did not cancel authorization")
	}
	return nil
}

func (r *Runtime) AuthStatus(ctx model.Context, request model.AuthStatusRequest) (model.AuthStatusResult, error) {
	if err := validateAuthAccount(request.AccountID); err != nil {
		return model.AuthStatusResult{}, err
	}
	if err := validateAuthProvider(request.Provider); err != nil {
		return model.AuthStatusResult{}, err
	}
	cctx, cancel := context.WithTimeout(asContext(ctx), 30*time.Second)
	defer cancel()
	if err := r.authPrerequisites(cctx, false); err != nil {
		return model.AuthStatusResult{}, err
	}
	var status authHelperStatus
	if err := r.authOneShotJSON(cctx, false, "status", []string{"--provider", string(request.Provider), "--account", request.AccountID}, &status); err != nil {
		return model.AuthStatusResult{}, err
	}
	if status.Protocol != authHelperProtocol || status.CheckedAt <= 0 {
		return model.AuthStatusResult{}, errors.New("authentication helper returned invalid status")
	}
	value := model.AuthStatus(status.Status)
	switch value {
	case model.AuthStatusAuthenticated, model.AuthStatusUnauthenticated, model.AuthStatusUnknown, model.AuthStatusUnavailable:
	default:
		return model.AuthStatusResult{}, errors.New("authentication helper returned invalid status")
	}
	return model.AuthStatusResult{Status: value, CheckedAt: time.Unix(status.CheckedAt, 0).UTC(), Message: status.Message}, nil
}

func (r *Runtime) LogoutAuth(ctx model.Context, request model.AuthLogoutRequest) (model.AuthLogoutResult, error) {
	if err := validateAuthAccount(request.AccountID); err != nil {
		return model.AuthLogoutResult{}, err
	}
	if err := validateAuthProvider(request.Provider); err != nil {
		return model.AuthLogoutResult{}, err
	}
	cctx, cancel := context.WithTimeout(asContext(ctx), 30*time.Second)
	defer cancel()
	if err := r.authPrerequisites(cctx, false); err != nil {
		return model.AuthLogoutResult{}, err
	}
	var result authHelperLogout
	if err := r.authOneShotJSON(cctx, false, "logout", []string{"--provider", string(request.Provider), "--account", request.AccountID}, &result); err != nil {
		return model.AuthLogoutResult{}, err
	}
	if result.Protocol != authHelperProtocol || !result.LocalCredentialsRemoved {
		return model.AuthLogoutResult{}, errors.New("authentication helper did not remove local credentials")
	}
	return model.AuthLogoutResult{
		LocalCredentialsRemoved: result.LocalCredentialsRemoved,
		ProviderLogoutConfirmed: result.ProviderLogoutConfirmed,
		Message:                 result.Message,
	}, nil
}

func (r *Runtime) authInspectContainer(ctx context.Context, name string) (authContainerInspect, error) {
	output, err := r.authRun(ctx, "inspect login container", "inspect", name)
	if err != nil {
		return authContainerInspect{}, err
	}
	var values []authContainerInspect
	decoder := json.NewDecoder(strings.NewReader(output))
	if err := decoder.Decode(&values); err != nil || len(values) != 1 {
		return authContainerInspect{}, errors.New("authentication container inspect response invalid")
	}
	return values[0], nil
}

func (r *Runtime) PrepareAuthCredentialCleanup(ctx model.Context, accountID string) (model.AuthCredentialCleanup, error) {
	if err := validateAuthAccount(accountID); err != nil {
		return model.AuthCredentialCleanup{}, err
	}
	cleanupID, err := authCleanupID()
	if err != nil {
		return model.AuthCredentialCleanup{}, err
	}
	cctx, cancel := context.WithTimeout(asContext(ctx), 30*time.Second)
	defer cancel()
	if err := r.authPrerequisites(cctx, false); err != nil {
		return model.AuthCredentialCleanup{}, err
	}
	var ack authHelperAck
	if err := r.authOneShotJSON(cctx, false, "cleanup-prepare", []string{
		"--account", accountID, "--cleanup", cleanupID, "--now", strconv.FormatInt(time.Now().Unix(), 10),
	}, &ack); err != nil {
		return model.AuthCredentialCleanup{}, err
	}
	if ack.Protocol != authHelperProtocol || !ack.Prepared {
		return model.AuthCredentialCleanup{}, errors.New("credential cleanup was not prepared")
	}
	return model.AuthCredentialCleanup{AccountID: accountID, CleanupID: cleanupID}, nil
}

func (r *Runtime) CommitAuthCredentialCleanup(ctx model.Context, cleanup model.AuthCredentialCleanup) error {
	return r.finishAuthCredentialCleanup(ctx, cleanup, "cleanup-commit")
}

func (r *Runtime) RollbackAuthCredentialCleanup(ctx model.Context, cleanup model.AuthCredentialCleanup) error {
	return r.finishAuthCredentialCleanup(ctx, cleanup, "cleanup-rollback")
}

func (r *Runtime) finishAuthCredentialCleanup(ctx model.Context, cleanup model.AuthCredentialCleanup, operation string) error {
	if err := validateAuthAccount(cleanup.AccountID); err != nil {
		return err
	}
	if !authCleanupPattern.MatchString(cleanup.CleanupID) {
		return errors.New("invalid credential cleanup identifier")
	}
	cctx, cancel := context.WithTimeout(asContext(ctx), 30*time.Second)
	defer cancel()
	if err := r.authPrerequisites(cctx, false); err != nil {
		return err
	}
	var ack authHelperAck
	if err := r.authOneShotJSON(cctx, false, operation, []string{"--account", cleanup.AccountID, "--cleanup", cleanup.CleanupID}, &ack); err != nil {
		return err
	}
	if ack.Protocol != authHelperProtocol || !ack.Completed {
		return errors.New("credential cleanup did not complete")
	}
	return nil
}

func (r *Runtime) ReconcileAuth(ctx model.Context, now time.Time) error {
	cctx, cancel := context.WithTimeout(asContext(ctx), 60*time.Second)
	defer cancel()
	output, err := r.authRun(cctx, "list login containers", "ps", "-aq", "--filter", "label=vibedock.auth_helper=true")
	if err != nil {
		return err
	}
	for _, containerID := range strings.Fields(output) {
		if !isDockerObjectID(containerID) {
			return errors.New("authentication container list response invalid")
		}
		inspect, inspectErr := r.authInspectContainer(cctx, containerID)
		if inspectErr != nil {
			continue
		}
		labels := inspect.Config.Labels
		if labels["vibedock.auth_helper"] != "true" || labels["vibedock.managed"] != "true" || labels["vibedock.auth_protocol"] != strconv.Itoa(authHelperProtocol) {
			continue
		}
		if operation := labels["vibedock.auth_operation"]; operation == "status" || operation == "logout" {
			expires, parseErr := strconv.ParseInt(labels["vibedock.auth_expires"], 10, 64)
			if inspect.State.Running && parseErr == nil && expires > now.Unix() {
				continue
			}
			_, _ = r.authRun(cctx, "remove stale authentication operation", "rm", "-f", containerID)
			continue
		}
		account, attempt := labels["vibedock.auth_account"], labels["vibedock.auth_attempt"]
		provider := model.AuthProvider(labels["vibedock.auth_provider"])
		ref := model.AuthAttemptRef{AccountID: account, AttemptID: attempt, Provider: provider}
		if validateAuthRef(ref) != nil {
			continue
		}
		expires, parseErr := strconv.ParseInt(labels["vibedock.auth_expires"], 10, 64)
		if parseErr != nil || expires > now.Unix() {
			continue
		}
		_, _ = r.authRun(cctx, "remove stale login container", "rm", "-f", containerID)
	}
	if err := r.authPrerequisites(cctx, false); err != nil {
		return err
	}
	var result struct {
		Protocol  int `json:"protocol"`
		Removed   int `json:"removed"`
		Recovered int `json:"recovered"`
	}
	if err := r.authOneShotJSON(cctx, false, "reconcile", []string{"--now", strconv.FormatInt(now.Unix(), 10)}, &result); err != nil {
		return err
	}
	if result.Protocol != authHelperProtocol || result.Removed < 0 || result.Recovered < 0 {
		return errors.New("authentication reconciliation response invalid")
	}
	return nil
}

func isDockerObjectID(value string) bool {
	if len(value) < 12 || len(value) > 64 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}
