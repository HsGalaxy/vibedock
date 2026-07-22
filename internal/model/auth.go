package model

import "time"

type AuthProvider string

const (
	AuthProviderCodex  AuthProvider = "codex"
	AuthProviderClaude AuthProvider = "claude"
)

type AuthAttemptState string

const (
	AuthAttemptStarting     AuthAttemptState = "starting"
	AuthAttemptAwaitingUser AuthAttemptState = "awaiting_user"
	AuthAttemptVerifying    AuthAttemptState = "verifying"
	AuthAttemptCompleted    AuthAttemptState = "completed"
	AuthAttemptCancelling   AuthAttemptState = "cancelling"
	AuthAttemptCancelled    AuthAttemptState = "cancelled"
	AuthAttemptExpired      AuthAttemptState = "expired"
	AuthAttemptFailed       AuthAttemptState = "failed"
)

func (state AuthAttemptState) Terminal() bool {
	switch state {
	case AuthAttemptCompleted, AuthAttemptCancelled, AuthAttemptExpired, AuthAttemptFailed:
		return true
	default:
		return false
	}
}

type AuthStatus string

const (
	AuthStatusAuthenticated   AuthStatus = "authenticated"
	AuthStatusUnauthenticated AuthStatus = "unauthenticated"
	AuthStatusLoginInProgress AuthStatus = "login_in_progress"
	AuthStatusUnknown         AuthStatus = "unknown"
	AuthStatusUnavailable     AuthStatus = "unavailable"
)

type AuthAttemptRef struct {
	AccountID string
	AttemptID string
	Provider  AuthProvider
}

type AuthStartRequest struct {
	AuthAttemptRef
	ExpiresAt time.Time
}

type AuthCompleteRequest struct {
	AuthAttemptRef
	AuthorizationResponse string
}

type AuthRuntimeAttempt struct {
	AuthAttemptRef
	State             AuthAttemptState
	VerificationURL   string
	UserCode          string
	ExpiresAt         time.Time
	PollAfter         time.Duration
	ErrorCode         string
	Message           string
	TerminalAvailable bool
}

type AuthStatusRequest struct {
	AccountID string
	Provider  AuthProvider
}

type AuthStatusResult struct {
	Status    AuthStatus
	CheckedAt time.Time
	Message   string
}

type AuthLogoutRequest struct {
	AccountID string
	Provider  AuthProvider
}

type AuthLogoutResult struct {
	LocalCredentialsRemoved bool
	ProviderLogoutConfirmed bool
	Message                 string
}

type AuthCredentialCleanup struct {
	AccountID string
	CleanupID string
}

type AuthRuntime interface {
	StartAuth(Context, AuthStartRequest) (AuthRuntimeAttempt, error)
	CompleteAuth(Context, AuthCompleteRequest) (AuthRuntimeAttempt, error)
	InspectAuth(Context, AuthAttemptRef) (AuthRuntimeAttempt, error)
	CancelAuth(Context, AuthAttemptRef) error
	AuthStatus(Context, AuthStatusRequest) (AuthStatusResult, error)
	LogoutAuth(Context, AuthLogoutRequest) (AuthLogoutResult, error)
	PrepareAuthCredentialCleanup(Context, string) (AuthCredentialCleanup, error)
	CommitAuthCredentialCleanup(Context, AuthCredentialCleanup) error
	RollbackAuthCredentialCleanup(Context, AuthCredentialCleanup) error
	ReconcileAuth(Context, time.Time) error
}
