package controlauth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
)

const (
	// SessionCookieName is shared with the Preview proxy's cookie filter. The
	// browser needs this host cookie on both ports, but Workspace applications
	// must never receive or replace it.
	SessionCookieName    = "vibedock_session"
	defaultSessionTTL    = 12 * time.Hour
	defaultFailureWindow = 15 * time.Minute
	defaultMaxFailures   = 5
	defaultBcryptCost    = 12
	defaultPasswordSlots = 2
	defaultMaxSessions   = 128
	defaultMaxFailKeys   = 4096
	maxRequestBytes      = 4096
	overflowFailureKey   = "\x00overflow"
)

var errCredentialExists = errors.New("control auth credentials already exist")

type Options struct {
	CredentialFile              string
	BootstrapTokenFile          string
	SessionTTL                  time.Duration
	FailureWindow               time.Duration
	MaxFailures                 int
	MaxFailureClients           int
	MaxSessions                 int
	MaxConcurrentPasswordChecks int
	BcryptCost                  int
	TrustProxyHeaders           bool
	Now                         func() time.Time
}

type Manager struct {
	mu                 sync.Mutex
	credentialFile     string
	bootstrapTokenFile string
	bootstrapToken     []byte
	credential         *diskCredential
	sessions           map[string]session
	failures           map[string][]time.Time
	passwordSlots      chan struct{}
	sessionTTL         time.Duration
	failureWindow      time.Duration
	maxFailures        int
	maxFailureClients  int
	maxSessions        int
	bcryptCost         int
	trustProxyHeaders  bool
	now                func() time.Time
	comparePassword    func([]byte, []byte) error
}

type diskCredential struct {
	Version      int       `json:"version"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"password_hash"`
	CreatedAt    time.Time `json:"created_at"`
}

type session struct {
	username string
	csrf     string
	expires  time.Time
	revoked  chan struct{}
}

type identityContextKey struct{}

type credentialRequest struct {
	Username   string `json:"username"`
	Password   string `json:"password"`
	SetupToken string `json:"setup_token,omitempty"`
}

type statusResponse struct {
	Initialized   bool   `json:"initialized"`
	Authenticated bool   `json:"authenticated"`
	Username      string `json:"username,omitempty"`
	CSRFToken     string `json:"csrf_token,omitempty"`
}

func New(options Options) (*Manager, error) {
	if strings.TrimSpace(options.CredentialFile) == "" {
		return nil, errors.New("control auth credential file is required")
	}
	if options.SessionTTL <= 0 {
		options.SessionTTL = defaultSessionTTL
	}
	if options.FailureWindow <= 0 {
		options.FailureWindow = defaultFailureWindow
	}
	if options.MaxFailures <= 0 {
		options.MaxFailures = defaultMaxFailures
	}
	if options.MaxFailureClients <= 0 {
		options.MaxFailureClients = defaultMaxFailKeys
	}
	if options.MaxSessions <= 0 {
		options.MaxSessions = defaultMaxSessions
	}
	if options.MaxConcurrentPasswordChecks <= 0 {
		options.MaxConcurrentPasswordChecks = defaultPasswordSlots
	}
	if options.BcryptCost == 0 {
		options.BcryptCost = defaultBcryptCost
	}
	if options.BcryptCost < bcrypt.MinCost || options.BcryptCost > bcrypt.MaxCost {
		return nil, fmt.Errorf("invalid bcrypt cost %d", options.BcryptCost)
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if strings.TrimSpace(options.BootstrapTokenFile) == "" {
		options.BootstrapTokenFile = filepath.Join(filepath.Dir(options.CredentialFile), "bootstrap.token")
	}
	if filepath.Clean(options.BootstrapTokenFile) == filepath.Clean(options.CredentialFile) {
		return nil, errors.New("bootstrap token file must differ from credential file")
	}

	m := &Manager{
		credentialFile:     options.CredentialFile,
		bootstrapTokenFile: options.BootstrapTokenFile,
		sessions:           make(map[string]session),
		failures:           make(map[string][]time.Time),
		passwordSlots:      make(chan struct{}, options.MaxConcurrentPasswordChecks),
		sessionTTL:         options.SessionTTL,
		failureWindow:      options.FailureWindow,
		maxFailures:        options.MaxFailures,
		maxFailureClients:  options.MaxFailureClients,
		maxSessions:        options.MaxSessions,
		bcryptCost:         options.BcryptCost,
		trustProxyHeaders:  options.TrustProxyHeaders,
		now:                options.Now,
		comparePassword:    bcrypt.CompareHashAndPassword,
	}
	credential, err := loadCredential(options.CredentialFile)
	if err != nil {
		return nil, err
	}
	m.credential = credential
	if credential == nil {
		m.bootstrapToken, err = loadOrCreateBootstrapToken(options.BootstrapTokenFile)
		if err != nil {
			return nil, err
		}
	} else if err := removeBootstrapToken(options.BootstrapTokenFile); err != nil {
		return nil, err
	}
	return m, nil
}

// Handler serves the public bootstrap endpoints and protects all API and
// workspace-proxy routes before forwarding them to next. Static GET/HEAD
// requests remain public so the embedded login page can render.
func (m *Manager) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w)
		switch r.URL.Path {
		case "/healthz":
			m.handleHealth(w, r)
			return
		case "/api/auth/status":
			m.handleStatus(w, r)
			return
		case "/api/auth/register":
			m.handleRegister(w, r)
			return
		case "/api/auth/login":
			m.handleLogin(w, r)
			return
		case "/api/auth/logout":
			m.handleLogout(w, r)
			return
		}

		apiRoute := r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/")
		protected := apiRoute || r.URL.Path == "/w" || strings.HasPrefix(r.URL.Path, "/w/")
		if !protected && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			next.ServeHTTP(w, r)
			return
		}
		m.serveAuthenticated(next, apiRoute, w, r)
	})
}

// SessionMiddleware can protect a separate tools listener while sharing the
// same sessions and cookie as Handler.
func (m *Manager) SessionMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setToolSecurityHeaders(w)
		m.serveAuthenticated(next, false, w, r)
	})
}

// UsernameFromContext returns the authenticated control-plane username.
func UsernameFromContext(ctx context.Context) (string, bool) {
	username, ok := ctx.Value(identityContextKey{}).(string)
	return username, ok && username != ""
}

func (m *Manager) Initialized() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.credential != nil
}

func (m *Manager) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeMethodNotAllowed(w, http.MethodGet)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = io.WriteString(w, `{"ok":true}`+"\n")
	}
}

func (m *Manager) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w, http.MethodGet)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now().UTC()
	m.pruneSessionsLocked(now)
	response := statusResponse{Initialized: m.credential != nil}
	if current, ok := m.sessionFromRequestLocked(r, now); ok {
		response.Authenticated = true
		response.Username = current.username
		response.CSRFToken = current.csrf
	}
	writeJSON(w, http.StatusOK, response)
}

func (m *Manager) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w, http.MethodPost)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if !m.sameOrigin(r) {
		writeError(w, http.StatusForbidden, "origin_not_allowed", "request must originate from this VibeDock instance")
		return
	}
	payload, err := decodeCredentialRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", err.Error())
		return
	}
	username, err := validateUsername(payload.Username)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", err.Error())
		return
	}
	if err := validatePassword(payload.Password); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", err.Error())
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.credential != nil {
		writeError(w, http.StatusConflict, "already_initialized", "administrator account already exists")
		return
	}
	providedSetupToken := strings.TrimSpace(payload.SetupToken)
	if providedSetupToken == "" || subtle.ConstantTimeCompare([]byte(providedSetupToken), m.bootstrapToken) != 1 {
		writeError(w, http.StatusForbidden, "setup_token_invalid", "valid setup token required")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(payload.Password), m.bcryptCost)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "registration_failed", "could not create administrator account")
		return
	}
	credential := &diskCredential{
		Version: 1, Username: username, PasswordHash: string(hash), CreatedAt: m.now().UTC(),
	}
	if err := persistCredential(m.credentialFile, credential); err != nil {
		if errors.Is(err, errCredentialExists) {
			winner, loadErr := loadCredential(m.credentialFile)
			if loadErr == nil && winner != nil {
				m.credential = winner
				m.bootstrapToken = nil
				_ = removeBootstrapToken(m.bootstrapTokenFile)
				writeError(w, http.StatusConflict, "already_initialized", "administrator account already exists")
				return
			}
		}
		writeError(w, http.StatusInternalServerError, "registration_failed", "could not save administrator account")
		return
	}
	m.credential = credential
	m.bootstrapToken = nil
	if err := removeBootstrapToken(m.bootstrapTokenFile); err != nil {
		writeError(w, http.StatusInternalServerError, "registration_failed", "administrator created but setup token cleanup failed; please log in")
		return
	}
	current, token, err := m.newSessionLocked()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "session_failed", "administrator created; please log in")
		return
	}
	m.setSessionCookie(w, r, token, current.expires)
	writeJSON(w, http.StatusCreated, statusResponse{
		Initialized: true, Authenticated: true, Username: current.username, CSRFToken: current.csrf,
	})
}

func (m *Manager) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w, http.MethodPost)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if !m.sameOrigin(r) {
		writeError(w, http.StatusForbidden, "origin_not_allowed", "request must originate from this VibeDock instance")
		return
	}
	payload, err := decodeCredentialRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", err.Error())
		return
	}

	clientKey := m.clientKey(r)
	m.mu.Lock()
	if m.credential == nil {
		m.mu.Unlock()
		writeError(w, http.StatusConflict, "registration_required", "create the administrator account first")
		return
	}
	now := m.now().UTC()
	key := m.failureKeyLocked(clientKey, now)
	if retryAfter := m.retryAfterLocked(key, now); retryAfter > 0 {
		m.mu.Unlock()
		w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())))
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many failed login attempts; try again later")
		return
	}
	select {
	case m.passwordSlots <- struct{}{}:
	default:
		m.mu.Unlock()
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, "login_busy", "too many password checks are already running")
		return
	}
	storedUsername := m.credential.Username
	storedPasswordHash := m.credential.PasswordHash
	comparePassword := m.comparePassword
	m.mu.Unlock()

	usernameOK := subtle.ConstantTimeCompare([]byte(payload.Username), []byte(storedUsername)) == 1
	passwordOK := func() bool {
		defer func() { <-m.passwordSlots }()
		return comparePassword([]byte(storedPasswordHash), []byte(payload.Password)) == nil
	}()

	m.mu.Lock()
	now = m.now().UTC()
	key = m.failureKeyLocked(clientKey, now)
	if !usernameOK || !passwordOK {
		m.recordFailureLocked(key, now)
		m.mu.Unlock()
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "invalid username or password")
		return
	}
	delete(m.failures, key)
	current, token, err := m.newSessionLocked()
	m.mu.Unlock()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "session_failed", "could not create login session")
		return
	}
	m.setSessionCookie(w, r, token, current.expires)
	writeJSON(w, http.StatusOK, statusResponse{
		Initialized: true, Authenticated: true, Username: current.username, CSRFToken: current.csrf,
	})
}

func (m *Manager) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w, http.MethodPost)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if !m.sameOrigin(r) {
		writeError(w, http.StatusForbidden, "origin_not_allowed", "request must originate from this VibeDock instance")
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now().UTC()
	current, token, ok := m.sessionTokenFromRequestLocked(r, now)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	if !validCSRF(r, current.csrf) {
		writeError(w, http.StatusForbidden, "csrf_invalid", "valid CSRF token required")
		return
	}
	m.revokeSessionLocked(token)
	m.clearSessionCookie(w, r)
	writeJSON(w, http.StatusOK, statusResponse{Initialized: m.credential != nil})
}

func (m *Manager) serveAuthenticated(next http.Handler, validateAPICSRF bool, w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	m.mu.Lock()
	now := m.now().UTC()
	current, _, ok := m.sessionTokenFromRequestLocked(r, now)
	m.mu.Unlock()
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	if isWebSocketUpgrade(r) && !m.sameOrigin(r) {
		writeError(w, http.StatusForbidden, "origin_not_allowed", "WebSocket must originate from this VibeDock instance")
		return
	}
	if validateAPICSRF && isMutation(r.Method) {
		if !m.sameOrigin(r) {
			writeError(w, http.StatusForbidden, "origin_not_allowed", "request must originate from this VibeDock instance")
			return
		}
		if !validCSRF(r, current.csrf) {
			writeError(w, http.StatusForbidden, "csrf_invalid", "valid CSRF token required")
			return
		}
	}
	identityContext := context.WithValue(r.Context(), identityContextKey{}, current.username)
	ctx, cancel := context.WithDeadline(identityContext, current.expires)
	defer cancel()
	go func() {
		select {
		case <-current.revoked:
			cancel()
		case <-ctx.Done():
		}
	}()
	next.ServeHTTP(w, r.WithContext(ctx))
}

func (m *Manager) newSessionLocked() (session, string, error) {
	token, err := randomToken()
	if err != nil {
		return session{}, "", err
	}
	csrf, err := randomToken()
	if err != nil {
		return session{}, "", err
	}
	now := m.now().UTC()
	m.pruneSessionsLocked(now)
	if len(m.sessions) >= m.maxSessions {
		var oldestToken string
		var oldestExpiry time.Time
		for existingToken, existing := range m.sessions {
			if oldestToken == "" || existing.expires.Before(oldestExpiry) {
				oldestToken = existingToken
				oldestExpiry = existing.expires
			}
		}
		m.revokeSessionLocked(oldestToken)
	}
	current := session{username: m.credential.Username, csrf: csrf, expires: now.Add(m.sessionTTL), revoked: make(chan struct{})}
	m.sessions[token] = current
	return current, token, nil
}

func (m *Manager) sessionFromRequestLocked(r *http.Request, now time.Time) (session, bool) {
	current, _, ok := m.sessionTokenFromRequestLocked(r, now)
	return current, ok
}

func (m *Manager) sessionTokenFromRequestLocked(r *http.Request, now time.Time) (session, string, bool) {
	// Cookie ordering prefers longer paths. A Preview script can create a
	// non-HttpOnly cookie with the same name and a longer path, so examine every
	// candidate instead of allowing an invalid shadow cookie to hide the real
	// host session.
	for _, cookie := range r.Cookies() {
		if cookie.Name != SessionCookieName || cookie.Value == "" {
			continue
		}
		current, ok := m.sessions[cookie.Value]
		if !ok {
			continue
		}
		if !now.Before(current.expires) {
			m.revokeSessionLocked(cookie.Value)
			continue
		}
		return current, cookie.Value, true
	}
	return session{}, "", false
}

func (m *Manager) pruneSessionsLocked(now time.Time) {
	for token, current := range m.sessions {
		if !now.Before(current.expires) {
			m.revokeSessionLocked(token)
		}
	}
}

func (m *Manager) revokeSessionLocked(token string) {
	current, ok := m.sessions[token]
	if !ok {
		return
	}
	delete(m.sessions, token)
	close(current.revoked)
}

func (m *Manager) recordFailureLocked(key string, now time.Time) {
	if _, exists := m.failures[key]; !exists && len(m.failures) >= m.maxFailureClients {
		if key != overflowFailureKey {
			key = overflowFailureKey
		}
		if _, overflowExists := m.failures[key]; !overflowExists {
			for existingKey := range m.failures {
				delete(m.failures, existingKey)
				break
			}
		}
	}
	cutoff := now.Add(-m.failureWindow)
	kept := m.failures[key][:0]
	for _, attemptedAt := range m.failures[key] {
		if attemptedAt.After(cutoff) {
			kept = append(kept, attemptedAt)
		}
	}
	if len(kept) < m.maxFailures {
		kept = append(kept, now)
	}
	m.failures[key] = kept
}

func (m *Manager) failureKeyLocked(clientKey string, now time.Time) string {
	if _, exists := m.failures[clientKey]; exists {
		return clientKey
	}
	if len(m.failures) < m.maxFailureClients {
		return clientKey
	}
	cutoff := now.Add(-m.failureWindow)
	for key, attempts := range m.failures {
		if len(attempts) == 0 || !attempts[len(attempts)-1].After(cutoff) {
			delete(m.failures, key)
		}
	}
	if len(m.failures) < m.maxFailureClients {
		return clientKey
	}
	return overflowFailureKey
}

func (m *Manager) retryAfterLocked(key string, now time.Time) time.Duration {
	cutoff := now.Add(-m.failureWindow)
	attempts := m.failures[key][:0]
	for _, attemptedAt := range m.failures[key] {
		if attemptedAt.After(cutoff) {
			attempts = append(attempts, attemptedAt)
		}
	}
	if len(attempts) == 0 {
		delete(m.failures, key)
		return 0
	}
	m.failures[key] = attempts
	if len(attempts) < m.maxFailures {
		return 0
	}
	retry := attempts[0].Add(m.failureWindow).Sub(now)
	if retry < time.Second {
		return time.Second
	}
	return time.Duration((retry+time.Second-1)/time.Second) * time.Second
}

func (m *Manager) clientKey(r *http.Request) string {
	if m.trustProxyHeaders {
		if forwarded := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0]); net.ParseIP(forwarded) != nil {
			return forwarded
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil && host != "" {
		return host
	}
	if parsed := net.ParseIP(r.RemoteAddr); parsed != nil {
		return parsed.String()
	}
	return "unknown"
}

func (m *Manager) sameOrigin(r *http.Request) bool {
	if fetchSite := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))); fetchSite != "" && fetchSite != "same-origin" {
		return false
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	if parsed.Scheme != m.requestScheme(r) {
		return false
	}
	return strings.EqualFold(parsed.Host, r.Host)
}

func (m *Manager) requestScheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	if m.trustProxyHeaders && strings.EqualFold(strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]), "https") {
		return "https"
	}
	return "http"
}

func (m *Manager) setSessionCookie(w http.ResponseWriter, r *http.Request, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookieName, Value: token, Path: "/", HttpOnly: true,
		Secure: m.requestScheme(r) == "https", SameSite: http.SameSiteStrictMode,
		Expires: expires, MaxAge: int(m.sessionTTL.Seconds()),
	})
}

func (m *Manager) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookieName, Value: "", Path: "/", HttpOnly: true,
		Secure: m.requestScheme(r) == "https", SameSite: http.SameSiteStrictMode,
		Expires: time.Unix(1, 0), MaxAge: -1,
	})
}

func loadCredential(path string) (*diskCredential, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect control auth credentials: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("control auth credential file must be a regular file")
	}
	if info.Size() > maxRequestBytes {
		return nil, errors.New("control auth credential file is too large")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("secure control auth credentials: %w", err)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open control auth credentials: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, maxRequestBytes))
	decoder.DisallowUnknownFields()
	var credential diskCredential
	if err := decoder.Decode(&credential); err != nil {
		return nil, fmt.Errorf("decode control auth credentials: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, fmt.Errorf("decode control auth credentials: %w", err)
	}
	if credential.Version != 1 {
		return nil, fmt.Errorf("unsupported control auth credential version %d", credential.Version)
	}
	if _, err := validateUsername(credential.Username); err != nil {
		return nil, fmt.Errorf("invalid stored control auth username: %w", err)
	}
	if _, err := bcrypt.Cost([]byte(credential.PasswordHash)); err != nil {
		return nil, errors.New("invalid stored control auth password hash")
	}
	return &credential, nil
}

func loadOrCreateBootstrapToken(path string) ([]byte, error) {
	for {
		token, exists, err := loadBootstrapToken(path)
		if err != nil {
			return nil, err
		}
		if exists {
			return token, nil
		}
		generated, err := randomToken()
		if err != nil {
			return nil, fmt.Errorf("generate bootstrap token: %w", err)
		}
		if err := createBootstrapToken(path, generated); errors.Is(err, os.ErrExist) {
			continue
		} else if err != nil {
			return nil, err
		}
		return []byte(generated), nil
	}
}

func loadBootstrapToken(path string) ([]byte, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("inspect bootstrap token: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, false, errors.New("bootstrap token file must be a regular file")
	}
	if info.Size() > 128 {
		return nil, false, errors.New("bootstrap token file is too large")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, false, fmt.Errorf("secure bootstrap token: %w", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false, fmt.Errorf("read bootstrap token: %w", err)
	}
	value := strings.TrimSpace(string(data))
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) != 32 {
		return nil, false, errors.New("bootstrap token must encode exactly 32 random bytes")
	}
	return []byte(value), true, nil
}

func createBootstrapToken(path, token string) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create bootstrap token directory: %w", err)
	}
	file, err := os.CreateTemp(directory, ".vibedock-bootstrap-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary bootstrap token: %w", err)
	}
	temporaryPath := file.Name()
	defer func() {
		_ = file.Close()
		_ = os.Remove(temporaryPath)
	}()
	if err := file.Chmod(0o600); err != nil {
		return fmt.Errorf("secure temporary bootstrap token: %w", err)
	}
	if _, err := io.WriteString(file, token+"\n"); err != nil {
		return fmt.Errorf("write bootstrap token: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync bootstrap token: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close bootstrap token: %w", err)
	}
	if err := os.Link(temporaryPath, path); err != nil {
		return err
	}
	return nil
}

func removeBootstrapToken(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove bootstrap token: %w", err)
	}
	return nil
}

func persistCredential(path string, credential *diskCredential) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create credential directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".vibedock-admin-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary credential file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("secure temporary credential file: %w", err)
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(credential); err != nil {
		temporary.Close()
		return fmt.Errorf("encode credentials: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync credentials: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close credentials: %w", err)
	}
	if err := os.Link(temporaryPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return errCredentialExists
		}
		return fmt.Errorf("install credentials: %w", err)
	}
	if directoryHandle, err := os.Open(directory); err == nil {
		_ = directoryHandle.Sync()
		_ = directoryHandle.Close()
	}
	return nil
}

func decodeCredentialRequest(r *http.Request) (credentialRequest, error) {
	if contentType := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0])); contentType != "application/json" {
		return credentialRequest{}, errors.New("Content-Type must be application/json")
	}
	decoder := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxRequestBytes))
	decoder.DisallowUnknownFields()
	var payload credentialRequest
	if err := decoder.Decode(&payload); err != nil {
		return credentialRequest{}, errors.New("request body must be valid JSON")
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return credentialRequest{}, errors.New("request body must contain one JSON object")
	}
	return payload, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("unexpected data after JSON object")
		}
		return err
	}
	return nil
}

func validateUsername(value string) (string, error) {
	value = strings.TrimSpace(value)
	length := utf8.RuneCountInString(value)
	if length < 3 || length > 64 || len(value) > 128 {
		return "", errors.New("username must contain 3 to 64 characters")
	}
	for _, character := range value {
		if unicode.IsControl(character) || unicode.IsSpace(character) || !(unicode.IsLetter(character) || unicode.IsNumber(character) || strings.ContainsRune("_.-", character)) {
			return "", errors.New("username may contain letters, numbers, dot, underscore, and hyphen")
		}
	}
	return value, nil
}

func validatePassword(value string) error {
	if utf8.RuneCountInString(value) < 12 {
		return errors.New("password must contain at least 12 characters")
	}
	if len(value) > 72 {
		return errors.New("password must not exceed 72 bytes")
	}
	return nil
}

func randomToken() (string, error) {
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

func validCSRF(r *http.Request, expected string) bool {
	provided := strings.TrimSpace(r.Header.Get("X-VibeDock-CSRF"))
	return provided != "" && subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}

func isMutation(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

func isWebSocketUpgrade(r *http.Request) bool {
	if !strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket") {
		return false
	}
	for _, value := range strings.Split(r.Header.Get("Connection"), ",") {
		if strings.EqualFold(strings.TrimSpace(value), "upgrade") {
			return true
		}
	}
	return false
}

func setSecurityHeaders(w http.ResponseWriter) {
	setToolSecurityHeaders(w)
	w.Header().Set("X-Frame-Options", "SAMEORIGIN")
}

func setToolSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
}

func writeMethodNotAllowed(w http.ResponseWriter, methods ...string) {
	w.Header().Set("Allow", strings.Join(methods, ", "))
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, map[string]string{"code": code, "error": message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
