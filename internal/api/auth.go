package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yhw5231/fluxgate/internal/admin"
	"github.com/yhw5231/fluxgate/internal/store"
)

// SessionStore persists console sessions and the administrator credential. It
// is satisfied by the SQLite store; tests substitute an in-memory double.
type SessionStore interface {
	LoadAdminAccount(context.Context) (admin.Account, error)
	CreateAdminSession(ctx context.Context, accountID int64, tokenHash string, issuedAt, expiresAt time.Time) error
	AuthenticateAdminSession(ctx context.Context, token string, now time.Time) (admin.Account, error)
	DeleteAdminSession(ctx context.Context, tokenHash string) error
	ChangeAdminPassword(ctx context.Context, accountID int64, passwordHash string) error
}

// Console session lifetime. A session is a convenience for the operator, not a
// long-lived API credential, so it expires on its own.
const (
	sessionLifetime = 12 * time.Hour

	// loginMaxAttempts and loginLockout bound online password guessing per
	// client address.
	loginMaxAttempts = 5
	loginLockout     = 15 * time.Minute
	// loginAttemptWindow is how long a failure stays counted against the client.
	loginAttemptWindow = 15 * time.Minute
)

// sessionCookieName is the console session cookie. The name carries no prefix
// because the console may be reached over plain HTTP inside a trusted network;
// setting __Host- would require Secure and silently break that deployment.
const sessionCookieName = "fluxgate_console_session"

// loginRequest is the JSON body of a console sign-in.
type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// loginLimiter tracks failed sign-in attempts per client key and locks a key
// out for a cooldown once it exceeds the allowance. The map is bounded by the
// number of distinct clients seen within the window, which the sweep below
// keeps from growing without limit.
type loginLimiter struct {
	mu       sync.Mutex
	attempts map[string]*loginAttempt
	now      func() time.Time
}

type loginAttempt struct {
	failures    int
	lastFailure time.Time
	lockedUntil time.Time
}

func newLoginLimiter(now func() time.Time) *loginLimiter {
	if now == nil {
		now = time.Now
	}
	return &loginLimiter{attempts: make(map[string]*loginAttempt), now: now}
}

// Allow reports whether a sign-in attempt from key may proceed, and how long
// the caller must wait when it may not.
func (l *loginLimiter) Allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweepLocked()

	attempt, ok := l.attempts[key]
	if !ok {
		return true, 0
	}
	now := l.now()
	if attempt.lockedUntil.After(now) {
		return false, attempt.lockedUntil.Sub(now)
	}
	if now.Sub(attempt.lastFailure) > loginAttemptWindow {
		delete(l.attempts, key)
		return true, 0
	}
	return true, 0
}

// Fail records a failed attempt and returns the lockout duration when this
// failure triggered one.
func (l *loginLimiter) Fail(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	attempt, ok := l.attempts[key]
	if !ok || now.Sub(attempt.lastFailure) > loginAttemptWindow {
		attempt = &loginAttempt{}
		l.attempts[key] = attempt
	}
	attempt.failures++
	attempt.lastFailure = now

	if attempt.failures >= loginMaxAttempts {
		attempt.lockedUntil = now.Add(loginLockout)
		attempt.failures = 0
		return loginLockout
	}
	return 0
}

// Succeed clears the failure history for a key after a valid sign-in.
func (l *loginLimiter) Succeed(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.attempts, key)
}

// sweepLocked drops entries that are neither locked nor within the attempt
// window, so a stream of distinct source addresses cannot grow the map forever.
func (l *loginLimiter) sweepLocked() {
	now := l.now()
	for key, attempt := range l.attempts {
		if attempt.lockedUntil.After(now) {
			continue
		}
		if now.Sub(attempt.lastFailure) > loginAttemptWindow {
			delete(l.attempts, key)
		}
	}
}

// consoleAuthenticationEnabled reports whether a console sign-in is possible.
// It is false when no administrator exists, which is a distinct condition from
// a wrong credential: the console shows a setup hint instead of a login form
// that cannot succeed.
func (s *Server) consoleAuthenticationEnabled(ctx context.Context) bool {
	if s.Sessions == nil {
		return false
	}
	_, err := s.Sessions.LoadAdminAccount(ctx)
	return err == nil
}

// handleSession reports the caller's console authentication state. It also
// tells an unauthenticated console whether an administrator exists yet, so the
// login page can explain an empty deployment instead of failing mysteriously.
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	authenticated := false
	username := ""
	changeRequired := false
	if account, ok := s.currentSession(r); ok {
		authenticated = true
		username = account.Username
		changeRequired = account.ChangeRequired
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated":   authenticated,
		"username":        username,
		"configured":      s.consoleAuthenticationEnabled(r.Context()),
		"change_required": changeRequired,
	})
}

// changePasswordRequest is the JSON body of a password change.
type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// handleChangePassword sets a new password for the signed-in account. It exists
// so an account created with the built-in default password can be made private,
// and it is reachable in that state even though every data endpoint is blocked.
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	if s.Sessions == nil {
		writeError(w, http.StatusServiceUnavailable, "console_auth_not_configured", "console authentication is not configured")
		return
	}

	account, ok := s.currentSession(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "a console session is required")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var request changePasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "request body must be a JSON object")
		return
	}

	// Re-authenticate with the current password so a stolen session cookie alone
	// cannot lock the real operator out of their own console.
	if err := admin.VerifyPassword(account.PasswordHash, request.CurrentPassword); err != nil {
		s.logAuthEvent(r, "console_password_change_failed", account.Username)
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "the current password is incorrect")
		return
	}
	if request.NewPassword == request.CurrentPassword {
		writeError(w, http.StatusBadRequest, "password_unchanged", "the new password must differ from the current one")
		return
	}
	// Sign-in requires a password, so an empty one could never be used again and
	// would lock the account out permanently. This is a reachability constraint,
	// not a strength rule: no minimum length is enforced beyond it.
	if request.NewPassword == "" {
		writeError(w, http.StatusBadRequest, "password_empty", "the new password must not be empty, or sign-in would be impossible")
		return
	}
	if err := admin.ValidatePassword(request.NewPassword); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_password", err.Error())
		return
	}

	hash, err := admin.HashPassword(request.NewPassword)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_password", err.Error())
		return
	}
	if err := s.Sessions.ChangeAdminPassword(r.Context(), account.ID, hash); err != nil {
		writeError(w, http.StatusInternalServerError, "password_change_failed", "could not store the new password")
		return
	}

	s.logAuthEvent(r, "console_password_changed", account.Username)
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "change_required": false})
}

// handleLogin verifies a username and password and issues a session cookie.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.Sessions == nil {
		writeError(w, http.StatusServiceUnavailable, "console_auth_not_configured", "console authentication is not configured")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var request loginRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "request body must be a JSON object")
		return
	}
	username := strings.TrimSpace(request.Username)
	password := request.Password
	if username == "" || password == "" {
		writeError(w, http.StatusBadRequest, "missing_credentials", "username and password are required")
		return
	}

	clientKey := clientIP(r)
	if clientKey == "" {
		clientKey = "unknown"
	}
	if allowed, retryAfter := s.limiter().Allow(clientKey); !allowed {
		w.Header().Set("Retry-After", retryAfterSeconds(retryAfter))
		writeError(w, http.StatusTooManyRequests, "too_many_attempts",
			"too many failed sign-in attempts; wait before trying again")
		s.logAuthEvent(r, "console_login_locked", username)
		return
	}

	account, err := s.Sessions.LoadAdminAccount(r.Context())
	if err != nil {
		if errors.Is(err, store.ErrNoAdminAccount) {
			writeError(w, http.StatusServiceUnavailable, "console_auth_not_configured",
				"no administrator account has been created for this gateway")
			return
		}
		writeError(w, http.StatusInternalServerError, "authentication_failed", "could not read the administrator account")
		return
	}

	// Verify the password even when the username is wrong so a valid username
	// cannot be distinguished by response timing.
	passwordErr := admin.VerifyPassword(account.PasswordHash, password)
	if account.Username != username || passwordErr != nil {
		lockout := s.limiter().Fail(clientKey)
		s.logAuthEvent(r, "console_login_failed", username)
		if lockout > 0 {
			w.Header().Set("Retry-After", retryAfterSeconds(lockout))
			writeError(w, http.StatusTooManyRequests, "too_many_attempts",
				"too many failed sign-in attempts; wait before trying again")
			return
		}
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "invalid username or password")
		return
	}

	token, tokenHash, err := admin.NewSessionToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "authentication_failed", "could not create a session")
		return
	}

	now := time.Now().UTC()
	expiresAt := now.Add(sessionLifetime)
	if err := s.Sessions.CreateAdminSession(r.Context(), account.ID, tokenHash, now, expiresAt); err != nil {
		writeError(w, http.StatusInternalServerError, "authentication_failed", "could not store the session")
		return
	}
	s.limiter().Succeed(clientKey)

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsHTTPS(r),
		Expires:  expiresAt,
		MaxAge:   int(sessionLifetime.Seconds()),
	})
	s.logAuthEvent(r, "console_login_succeeded", account.Username)
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated":   true,
		"username":        account.Username,
		"change_required": account.ChangeRequired,
	})
}

// handleLogout revokes the caller's session and clears the cookie.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if s.Sessions != nil {
		if cookie, err := r.Cookie(sessionCookieName); err == nil && strings.TrimSpace(cookie.Value) != "" {
			if err := s.Sessions.DeleteAdminSession(r.Context(), admin.HashSessionToken(cookie.Value)); err != nil {
				s.logAuthEvent(r, "console_logout_failed", "")
			}
		}
	}
	// Clear with the same attributes used to set it, or the browser keeps the
	// original cookie and the console stays signed in.
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsHTTPS(r),
		MaxAge:   -1,
		Expires:  time.Unix(0, 0).UTC(),
	})
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": false})
}

// currentSession resolves the request's session cookie to an account.
func (s *Server) currentSession(r *http.Request) (admin.Account, bool) {
	if s.Sessions == nil {
		return admin.Account{}, false
	}
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || strings.TrimSpace(cookie.Value) == "" {
		return admin.Account{}, false
	}
	account, err := s.Sessions.AuthenticateAdminSession(r.Context(), cookie.Value, time.Now().UTC())
	if err != nil {
		return admin.Account{}, false
	}
	return account, true
}

// authenticateConsole authorizes a management request. A console session
// issued by a sign-in is preferred; the configured management token remains
// accepted so existing automation and scripts keep working unchanged.
//
// A session whose account still holds the built-in default password is refused
// here rather than at the UI, so the gate cannot be bypassed by calling the
// endpoint directly. Such an account can reach only the session and
// change-password routes, which is what lets the operator set a real password.
func (s *Server) authenticateConsole(w http.ResponseWriter, r *http.Request) bool {
	if account, ok := s.currentSession(r); ok {
		if account.ChangeRequired {
			writeError(w, http.StatusForbidden, "password_change_required",
				"the console password must be changed before any data can be read")
			return false
		}
		return true
	}
	return s.authenticateManagement(w, r)
}

// logAuthEvent records a sign-in outcome without ever including a credential.
// The username is logged because it is not secret and is needed to see which
// account is being attacked; the password and session token never are.
func (s *Server) logAuthEvent(r *http.Request, event, username string) {
	if s.Logger == nil {
		return
	}
	attributes := []any{
		"event", event,
		"client_ip", clientIP(r),
	}
	if username != "" {
		attributes = append(attributes, "username", username)
	}
	s.Logger.Info(event, attributes...)
}

// requestIsHTTPS reports whether the browser reached the gateway over TLS. A
// gateway behind a TLS-terminating proxy sees plain HTTP, so the proxy's
// forwarding header decides, and a Secure cookie would otherwise never be set.
func requestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	for _, header := range []string{"X-Forwarded-Proto", "X-Forwarded-Protocol", "X-Url-Scheme"} {
		scheme := strings.TrimSpace(strings.ToLower(r.Header.Get(header)))
		if scheme == "" {
			continue
		}
		// A proxy chain lists the client-facing scheme first.
		if first, _, found := strings.Cut(scheme, ","); found {
			scheme = strings.TrimSpace(first)
		}
		if scheme == "https" || scheme == "wss" {
			return true
		}
		if scheme == "http" || scheme == "ws" {
			return false
		}
	}
	return false
}

// retryAfterSeconds formats a cooldown as the whole seconds an HTTP Retry-After
// header takes. It always reports at least one second so a client never sees a
// zero that reads as "retry immediately".
func retryAfterSeconds(duration time.Duration) string {
	seconds := int(duration.Seconds())
	if seconds < 1 {
		seconds = 1
	}
	return strconv.Itoa(seconds)
}
