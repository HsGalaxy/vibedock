package docker

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"vibedock/internal/model"
)

const (
	authHelperDefaultImage = "vibedock/workspace:local"
	authHelperEntrypoint   = "/usr/local/bin/vibedock-auth-helper"
	authHelperProtocol     = 1
	authCredentialsVolume  = "vibedock-credentials"
	authTerminalPort       = 7681
	authOutputLimit        = 1 << 20
)

var (
	authAccountPattern  = regexp.MustCompile(`^[a-z0-9]{12}$`)
	authAttemptPattern  = regexp.MustCompile(`^[a-z0-9]{24}$`)
	authCleanupPattern  = regexp.MustCompile(`^[a-z0-9]{32}$`)
	authUserCodePattern = regexp.MustCompile(`^[A-Z0-9-]{4,32}$`)
	authTerminalSecrets sync.Map
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

func authContainerName(ref model.AuthAttemptRef) string {
	return "vibedock-auth-" + ref.AttemptID
}

func authSecretKey(ref model.AuthAttemptRef) string {
	return ref.AccountID + "/" + ref.AttemptID + "/" + string(ref.Provider)
}

func authTerminalPassword() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", errors.New("generate authentication terminal credential")
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
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
	if _, err := r.authRun(ctx, "create credentials volume", "volume", "create", authCredentialsVolume); err != nil {
		return err
	}
	return r.authProtocol(ctx)
}

func (r *Runtime) authProtocol(ctx context.Context) error {
	var manifest authHelperManifest
	if err := r.authOneShotJSON(ctx, false, "protocol", nil, &manifest); err != nil {
		return err
	}
	if manifest.Protocol != authHelperProtocol || manifest.CodexVersion != "0.144.6" || manifest.ClaudeVersion != "2.1.215" || manifest.TTydVersion != "1.7.7" {
		return errors.New("authentication helper protocol incompatible")
	}
	return nil
}

func (r *Runtime) authOneShotJSON(ctx context.Context, network bool, operation string, operationArgs []string, result any) error {
	args := []string{"run", "--rm"}
	if network && (operation == "status" || operation == "logout") {
		accountID, accountOK := authOperationArg(operationArgs, "--account")
		provider, providerOK := authOperationArg(operationArgs, "--provider")
		if !accountOK || !providerOK || validateAuthAccount(accountID) != nil || validateAuthProvider(model.AuthProvider(provider)) != nil {
			return errors.New("invalid authentication helper operation")
		}
		args = append(args,
			"--label", "vibedock.managed=true",
			"--label", "vibedock.auth_helper=true",
			"--label", "vibedock.auth_operation="+operation,
			"--label", "vibedock.auth_account="+accountID,
			"--label", "vibedock.auth_provider="+provider,
			"--label", "vibedock.auth_expires="+strconv.FormatInt(time.Now().Add(2*time.Minute).Unix(), 10),
			"--label", "vibedock.auth_protocol="+strconv.Itoa(authHelperProtocol),
		)
	}
	args = append(args, authOneShotDockerArgs(r.Network, network)...)
	args = append(args, "--entrypoint", authHelperEntrypoint, authHelperImage(), operation)
	args = append(args, operationArgs...)
	output, err := r.authRun(ctx, operation, args...)
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

func authOperationArg(args []string, name string) (string, bool) {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == name {
			return args[index+1], true
		}
	}
	return "", false
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
	if err := r.authPrerequisites(cctx, true); err != nil {
		return model.AuthRuntimeAttempt{}, err
	}
	ref := request.AuthAttemptRef
	name := authContainerName(ref)
	password := ""
	if ref.Provider == model.AuthProviderClaude {
		var err error
		password, err = authTerminalPassword()
		if err != nil {
			return model.AuthRuntimeAttempt{}, err
		}
	}
	args := []string{"create", "--name", name}
	args = append(args, authBaseDockerArgs(r.Network)...)
	args = append(args,
		"--label", "vibedock.managed=true",
		"--label", "vibedock.auth_helper=true",
		"--label", "vibedock.auth_account="+ref.AccountID,
		"--label", "vibedock.auth_attempt="+ref.AttemptID,
		"--label", "vibedock.auth_provider="+string(ref.Provider),
		"--label", "vibedock.auth_expires="+strconv.FormatInt(request.ExpiresAt.Unix(), 10),
		"--label", "vibedock.auth_protocol="+strconv.Itoa(authHelperProtocol),
	)
	args = append(args,
		"--entrypoint", authHelperEntrypoint,
		authHelperImage(),
		"login",
		"--provider", string(ref.Provider),
		"--account", ref.AccountID,
		"--attempt", ref.AttemptID,
		"--expires-at", strconv.FormatInt(request.ExpiresAt.Unix(), 10),
	)
	if _, err := r.authRun(cctx, "create login container", args...); err != nil {
		return model.AuthRuntimeAttempt{}, err
	}
	created := true
	defer func() {
		if created {
			_, _ = r.authRun(context.Background(), "remove login container", "rm", "-f", name)
		}
	}()
	if _, err := r.authRun(cctx, "start login container", "start", name); err != nil {
		return model.AuthRuntimeAttempt{}, err
	}
	if password != "" {
		execArgs := []string{"exec", "-i", "--user", "1001:1001", name, authHelperEntrypoint, "terminal-secret"}
		if _, err := r.authRunWithInput(cctx, "stage login terminal credential", password+"\n", execArgs...); err != nil {
			return model.AuthRuntimeAttempt{}, err
		}
		authTerminalSecrets.Store(authSecretKey(ref), password)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		attempt, err := r.inspectAuth(cctx, ref)
		if err == nil {
			created = false
			return attempt, nil
		}
		if time.Now().After(deadline) || cctx.Err() != nil {
			if password != "" {
				authTerminalSecrets.Delete(authSecretKey(ref))
			}
			return model.AuthRuntimeAttempt{}, errors.New("authentication helper did not become ready")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (r *Runtime) InspectAuth(ctx model.Context, ref model.AuthAttemptRef) (model.AuthRuntimeAttempt, error) {
	if err := validateAuthRef(ref); err != nil {
		return model.AuthRuntimeAttempt{}, err
	}
	cctx, cancel := context.WithTimeout(asContext(ctx), 10*time.Second)
	defer cancel()
	attempt, err := r.inspectAuth(cctx, ref)
	if err != nil {
		return model.AuthRuntimeAttempt{}, err
	}
	if attempt.State.Terminal() {
		authTerminalSecrets.Delete(authSecretKey(ref))
	}
	return attempt, nil
}

func (r *Runtime) inspectAuth(ctx context.Context, ref model.AuthAttemptRef) (model.AuthRuntimeAttempt, error) {
	var state authHelperState
	if err := r.authOneShotJSON(ctx, false, "inspect", []string{
		"--provider", string(ref.Provider), "--account", ref.AccountID, "--attempt", ref.AttemptID,
	}, &state); err != nil {
		return model.AuthRuntimeAttempt{}, err
	}
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
	if attemptState == model.AuthAttemptAwaitingUser && ref.Provider == model.AuthProviderCodex {
		if err := validateCodexVerificationURL(state.VerificationURL); err != nil || !authUserCodePattern.MatchString(state.UserCode) {
			return model.AuthRuntimeAttempt{}, errors.New("authentication helper returned unsafe device login data")
		}
	}
	if attemptState.Terminal() && (state.VerificationURL != "" || state.UserCode != "" || state.TerminalAvailable) {
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

func validateCodexVerificationURL(value string) error {
	if len(value) < 12 || len(value) > 256 {
		return errors.New("invalid verification URL")
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "auth.openai.com" || parsed.Path != "/codex/device" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return errors.New("invalid verification URL")
	}
	if parsed.Port() != "" && parsed.Port() != "443" {
		return errors.New("invalid verification URL")
	}
	return nil
}

func (r *Runtime) CancelAuth(ctx model.Context, ref model.AuthAttemptRef) error {
	if err := validateAuthRef(ref); err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(asContext(ctx), 15*time.Second)
	defer cancel()
	name := authContainerName(ref)
	_, stopErr := r.authRun(cctx, "stop login container", "stop", "--time", "5", name)
	_, removeErr := r.authRun(cctx, "remove login container", "rm", "-f", name)
	authTerminalSecrets.Delete(authSecretKey(ref))
	if stopErr != nil && removeErr != nil {
		return errors.New("authentication helper cancellation failed")
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
	cctx, cancel := context.WithTimeout(asContext(ctx), 45*time.Second)
	defer cancel()
	if err := r.authPrerequisites(cctx, true); err != nil {
		return model.AuthStatusResult{}, err
	}
	var status authHelperStatus
	if err := r.authOneShotJSON(cctx, true, "status", []string{"--provider", string(request.Provider), "--account", request.AccountID}, &status); err != nil {
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
	cctx, cancel := context.WithTimeout(asContext(ctx), 60*time.Second)
	defer cancel()
	if err := r.authPrerequisites(cctx, true); err != nil {
		return model.AuthLogoutResult{}, err
	}
	var result authHelperLogout
	if err := r.authOneShotJSON(cctx, true, "logout", []string{"--provider", string(request.Provider), "--account", request.AccountID}, &result); err != nil {
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

func (r *Runtime) AuthTerminal(ctx model.Context, ref model.AuthAttemptRef) (model.AuthTerminalTarget, error) {
	if err := validateAuthRef(ref); err != nil {
		return model.AuthTerminalTarget{}, err
	}
	if ref.Provider != model.AuthProviderClaude {
		return model.AuthTerminalTarget{}, errors.New("authentication attempt has no terminal")
	}
	passwordValue, ok := authTerminalSecrets.Load(authSecretKey(ref))
	if !ok {
		return model.AuthTerminalTarget{}, errors.New("authentication terminal is unavailable")
	}
	password, ok := passwordValue.(string)
	if !ok || password == "" {
		return model.AuthTerminalTarget{}, errors.New("authentication terminal is unavailable")
	}
	cctx, cancel := context.WithTimeout(asContext(ctx), 10*time.Second)
	defer cancel()
	inspect, err := r.authInspectContainer(cctx, authContainerName(ref))
	if err != nil {
		return model.AuthTerminalTarget{}, err
	}
	if !inspect.State.Running || inspect.State.Status != "running" || !authLabelsMatch(inspect.Config.Labels, ref) {
		return model.AuthTerminalTarget{}, errors.New("authentication terminal is unavailable")
	}
	ip := ""
	if network, ok := inspect.NetworkSettings.Networks[r.Network]; ok {
		ip = network.IPAddress
	}
	if net.ParseIP(ip) == nil {
		return model.AuthTerminalTarget{}, errors.New("authentication terminal has no reachable address")
	}
	return model.AuthTerminalTarget{URL: "http://" + net.JoinHostPort(ip, strconv.Itoa(authTerminalPort)), Username: "vibe", Password: password}, nil
}

func authLabelsMatch(labels map[string]string, ref model.AuthAttemptRef) bool {
	return labels["vibedock.managed"] == "true" &&
		labels["vibedock.auth_helper"] == "true" &&
		labels["vibedock.auth_account"] == ref.AccountID &&
		labels["vibedock.auth_attempt"] == ref.AttemptID &&
		labels["vibedock.auth_provider"] == string(ref.Provider) &&
		labels["vibedock.auth_protocol"] == strconv.Itoa(authHelperProtocol)
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
		authTerminalSecrets.Delete(authSecretKey(ref))
	}
	if _, err := r.authRun(cctx, "create credentials volume", "volume", "create", authCredentialsVolume); err != nil {
		return err
	}
	if err := r.authProtocol(cctx); err != nil {
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
