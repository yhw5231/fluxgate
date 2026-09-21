package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yhw5231/fluxgate/internal/admin"
	"github.com/yhw5231/fluxgate/internal/store"
)

// fakeSessionStore is an in-memory SessionStore so handler tests do not need a
// SQLite file. It mirrors the real store's contract: only token hashes are
// retained, and expired sessions do not authenticate.
type fakeSessionStore struct {
	mu       sync.Mutex
	account  admin.Account
	hasUser  bool
	sessions map[string]fakeSession
	now      func() time.Time
}

type fakeSession struct {
	accountID int64
	expiresAt time.Time
}

func newFakeSessionStore(username, password string) *fakeSessionStore {
	hash, err := admin.HashPassword(password)
	if err != nil {
		panic(err)
	}
	return &fakeSessionStore{
		account: admin.Account{
			ID:           1,
			Username:     username,
			PasswordHash: hash,
			CreatedAt:    time.Now().UTC(),
		},
		hasUser:  true,
		sessions: make(map[string]fakeSession),
		now:      time.Now,
	}
}

func (f *fakeSessionStore) LoadAdminAccount(context.Context) (admin.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.hasUser {
		return admin.Account{}, store.ErrNoAdminAccount
	}
	return f.account, nil
}

func (f *fakeSessionStore) CreateAdminSession(_ context.Context, accountID int64, tokenHash string, _ time.Time, expiresAt time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions[tokenHash] = fakeSession{accountID: accountID, expiresAt: expiresAt}
	return nil
}

func (f *fakeSessionStore) AuthenticateAdminSession(_ context.Context, token string, now time.Time) (admin.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	session, ok := f.sessions[admin.HashSessionToken(token)]
	if !ok {
		return admin.Account{}, store.ErrSessionNotFound
	}
	if !now.UTC().Before(session.expiresAt) {
		return admin.Account{}, store.ErrSessionNotFound
	}
	return f.account, nil
}

func (f *fakeSessionStore) DeleteAdminSession(_ context.Context, tokenHash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sessions, tokenHash)
	return nil
}

// sessionCount reports how many live sessions the store holds.
func (f *fakeSessionStore) sessionCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sessions)
}

func submitLogin(t *testing.T, handler http.Handler, username, password string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]string{"username": username, "password": password})
	if err != nil {
		t.Fatalf("marshal login body: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/management/login", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// sessionCookie extracts the console session cookie a login response set.
func sessionCookie(t *testing.T, response *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == sessionCookieName {
			return cookie
		}
	}
	t.Fatalf("no %s cookie in the login response", sessionCookieName)
	return nil
}

func TestLoginIssuesSessionCookieAndAuthorizesManagement(t *testing.T) {
	sessions := newFakeSessionStore("admin", "correct-horse-battery-staple")
	server := &Server{Sessions: sessions, ManagementToken: "management-secret"}
	handler := server.Handler()

	response := submitLogin(t, handler, "admin", "correct-horse-battery-staple")
	if response.Code != http.StatusOK {
		t.Fatalf("login status = %d, want 200 (body %s)", response.Code, response.Body.String())
	}

	cookie := sessionCookie(t, response)
	if !cookie.HttpOnly {
		t.Error("session cookie is not HttpOnly")
	}
	if cookie.Path != "/" {
		t.Errorf("cookie Path = %q, want /", cookie.Path)
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie SameSite = %v, want Lax", cookie.SameSite)
	}
	if cookie.Value == "" {
		t.Error("session cookie is empty")
	}

	// The session alone, with no management token, must authorize management.
	request := httptest.NewRequest(http.MethodGet, "/management/status", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookie.Value})
	statusResponse := httptest.NewRecorder()
	handler.ServeHTTP(statusResponse, request)
	if statusResponse.Code != http.StatusOK {
		t.Fatalf("management status with a session = %d, want 200", statusResponse.Code)
	}
}

func TestLoginRejectsWrongCredentials(t *testing.T) {
	sessions := newFakeSessionStore("admin", "correct-horse-battery-staple")

	for _, testCase := range []struct{ name, username, password string }{
		{name: "wrong password", username: "admin", password: "wrong-but-long-enough"},
		{name: "wrong username", username: "root", password: "correct-horse-battery-staple"},
		{name: "both wrong", username: "root", password: "nope"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// A fresh server per case keeps the limiter from carrying failures over.
			response := submitLogin(t, (&Server{Sessions: sessions}).Handler(), testCase.username, testCase.password)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (body %s)", response.Code, response.Body.String())
			}
			if sessions.sessionCount() != 0 {
				t.Fatal("a session was created for a failed sign-in")
			}
		})
	}
}

// A wrong username must not reveal whether the account exists, so the error for
// an unknown user is identical to the one for a bad password.
func TestLoginDoesNotDistinguishUnknownUsername(t *testing.T) {
	sessions := newFakeSessionStore("admin", "correct-horse-battery-staple")

	unknown := submitLogin(t, (&Server{Sessions: sessions}).Handler(), "nosuchuser", "correct-horse-battery-staple")
	wrongPassword := submitLogin(t, (&Server{Sessions: sessions}).Handler(), "admin", "a-wrong-password-here")

	if unknown.Code != wrongPassword.Code {
		t.Fatalf("unknown username = %d, wrong password = %d; they must match", unknown.Code, wrongPassword.Code)
	}
	if unknown.Body.String() != wrongPassword.Body.String() {
		t.Fatalf("response bodies differ:\n unknown username: %s\n wrong password:   %s",
			unknown.Body.String(), wrongPassword.Body.String())
	}
}

func TestLoginRejectsMalformedRequests(t *testing.T) {
	sessions := newFakeSessionStore("admin", "correct-horse-battery-staple")
	handler := (&Server{Sessions: sessions}).Handler()

	cases := []struct {
		name string
		body string
	}{
		{name: "not json", body: "{"},
		{name: "missing password", body: `{"username":"admin"}`},
		{name: "missing username", body: `{"password":"correct-horse-battery-staple"}`},
		{name: "blank username", body: `{"username":"   ","password":"correct-horse-battery-staple"}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/management/login", strings.NewReader(testCase.body))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)

			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", response.Code, response.Body.String())
			}
		})
	}
}

// The console must explain an empty deployment instead of failing a login that
// can never succeed.
func TestLoginReportsMissingAdministrator(t *testing.T) {
	sessions := newFakeSessionStore("admin", "correct-horse-battery-staple")
	sessions.hasUser = false
	server := &Server{Sessions: sessions}
	handler := server.Handler()

	response := submitLogin(t, handler, "admin", "correct-horse-battery-staple")
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("login status = %d, want 503", response.Code)
	}
	if !strings.Contains(response.Body.String(), "console_auth_not_configured") {
		t.Fatalf("body = %s, want console_auth_not_configured", response.Body.String())
	}

	sessionResponse := httptest.NewRecorder()
	handler.ServeHTTP(sessionResponse, httptest.NewRequest(http.MethodGet, "/management/session", nil))
	if sessionResponse.Code != http.StatusOK {
		t.Fatalf("session status = %d, want 200", sessionResponse.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(sessionResponse.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode session body: %v", err)
	}
	if body["configured"] != false {
		t.Fatalf("configured = %v, want false", body["configured"])
	}
	if body["authenticated"] != false {
		t.Fatalf("authenticated = %v, want false", body["authenticated"])
	}
}

func TestSessionEndpointReportsAuthenticatedUser(t *testing.T) {
	sessions := newFakeSessionStore("admin", "correct-horse-battery-staple")
	handler := (&Server{Sessions: sessions}).Handler()

	// Unauthenticated.
	anonymous := httptest.NewRecorder()
	handler.ServeHTTP(anonymous, httptest.NewRequest(http.MethodGet, "/management/session", nil))
	var anonymousBody map[string]any
	if err := json.Unmarshal(anonymous.Body.Bytes(), &anonymousBody); err != nil {
		t.Fatalf("decode anonymous body: %v", err)
	}
	if anonymousBody["authenticated"] != false {
		t.Fatalf("authenticated = %v, want false", anonymousBody["authenticated"])
	}
	if anonymousBody["configured"] != true {
		t.Fatalf("configured = %v, want true", anonymousBody["configured"])
	}

	// Authenticated.
	cookie := sessionCookie(t, submitLogin(t, handler, "admin", "correct-horse-battery-staple"))
	request := httptest.NewRequest(http.MethodGet, "/management/session", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookie.Value})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["authenticated"] != true {
		t.Fatalf("authenticated = %v, want true", body["authenticated"])
	}
	if body["username"] != "admin" {
		t.Fatalf("username = %v, want admin", body["username"])
	}
}

func TestLogoutRevokesSession(t *testing.T) {
	sessions := newFakeSessionStore("admin", "correct-horse-battery-staple")
	handler := (&Server{Sessions: sessions}).Handler()

	cookie := sessionCookie(t, submitLogin(t, handler, "admin", "correct-horse-battery-staple"))
	if sessions.sessionCount() != 1 {
		t.Fatalf("sessions after login = %d, want 1", sessions.sessionCount())
	}

	request := httptest.NewRequest(http.MethodPost, "/management/logout", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookie.Value})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("logout status = %d, want 200", response.Code)
	}
	if sessions.sessionCount() != 0 {
		t.Fatal("logout did not revoke the session server-side")
	}

	// The cleared cookie is what the browser will store next.
	cleared := response.Result().Cookies()
	if len(cleared) == 0 {
		t.Fatal("logout did not clear the session cookie")
	}
	for _, c := range cleared {
		if c.Name == sessionCookieName && c.MaxAge >= 0 {
			t.Fatalf("logout cookie MaxAge = %d, want a negative value to delete it", c.MaxAge)
		}
	}

	// The revoked token must not authorize anything afterwards.
	replay := httptest.NewRequest(http.MethodGet, "/management/status", nil)
	replay.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookie.Value})
	replayResponse := httptest.NewRecorder()
	handler.ServeHTTP(replayResponse, replay)
	if replayResponse.Code != http.StatusUnauthorized {
		t.Fatalf("replayed revoked session = %d, want 401", replayResponse.Code)
	}
}

func TestManagementStillAcceptsConfiguredToken(t *testing.T) {
	sessions := newFakeSessionStore("admin", "correct-horse-battery-staple")
	handler := (&Server{Sessions: sessions, ManagementToken: "management-secret"}).Handler()

	for _, testCase := range []struct{ name, header, value string }{
		{name: "bearer", header: "Authorization", value: "Bearer management-secret"},
		{name: "x-management-token", header: "X-Management-Token", value: "management-secret"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/management/status", nil)
			request.Header.Set(testCase.header, testCase.value)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)

			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", response.Code, response.Body.String())
			}
		})
	}

	// A wrong token is still rejected.
	request := httptest.NewRequest(http.MethodGet, "/management/status", nil)
	request.Header.Set("Authorization", "Bearer wrong-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d, want 401", response.Code)
	}
}

// A console whose only credential is a session must not report the management
// token as unconfigured, or the console would show a setup banner forever.
func TestManagementWithoutTokenButWithSessionsReturns401Not503(t *testing.T) {
	sessions := newFakeSessionStore("admin", "correct-horse-battery-staple")
	handler := (&Server{Sessions: sessions}).Handler()

	request := httptest.NewRequest(http.MethodGet, "/management/status", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.Code)
	}
}

func TestManagementReturns503WhenNothingIsConfigured(t *testing.T) {
	handler := (&Server{}).Handler()

	request := httptest.NewRequest(http.MethodGet, "/management/status", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.Code)
	}
	if !strings.Contains(response.Body.String(), "management_auth_not_configured") {
		t.Fatalf("body = %s, want management_auth_not_configured", response.Body.String())
	}
}

// Repeated failures from one address must lock out further attempts.
func TestLoginLocksOutAfterRepeatedFailures(t *testing.T) {
	sessions := newFakeSessionStore("admin", "correct-horse-battery-staple")
	server := &Server{Sessions: sessions}
	handler := server.Handler()

	var last *httptest.ResponseRecorder
	for i := 0; i < loginMaxAttempts; i++ {
		last = submitLogin(t, handler, "admin", "wrong-password-entirely")
	}
	if last.Code != http.StatusTooManyRequests {
		t.Fatalf("status after %d failures = %d, want 429", loginMaxAttempts, last.Code)
	}
	if last.Header().Get("Retry-After") == "" {
		t.Fatal("a lockout response must carry Retry-After")
	}

	// The correct password is refused while the lockout holds.
	locked := submitLogin(t, handler, "admin", "correct-horse-battery-staple")
	if locked.Code != http.StatusTooManyRequests {
		t.Fatalf("status with correct credentials during lockout = %d, want 429", locked.Code)
	}
}

func TestLoginLimiterRecoversAfterWindow(t *testing.T) {
	current := time.Now()
	limiter := newLoginLimiter(func() time.Time { return current })

	for i := 0; i < loginMaxAttempts; i++ {
		limiter.Fail("203.0.113.9")
	}
	if allowed, _ := limiter.Allow("203.0.113.9"); allowed {
		t.Fatal("the limiter allowed a locked-out key")
	}

	// Past the lockout, the key is allowed again.
	current = current.Add(loginLockout + time.Minute)
	if allowed, _ := limiter.Allow("203.0.113.9"); !allowed {
		t.Fatal("the limiter stayed locked after the cooldown elapsed")
	}
}

func TestLoginLimiterForgetsSuccessesAndSweepsStaleEntries(t *testing.T) {
	current := time.Now()
	limiter := newLoginLimiter(func() time.Time { return current })

	// A successful sign-in clears prior failures for that key.
	limiter.Fail("key-a")
	limiter.Succeed("key-a")
	if allowed, _ := limiter.Allow("key-a"); !allowed {
		t.Fatal("a key was locked after a successful sign-in")
	}

	// Stale entries for other keys are swept rather than retained forever.
	limiter.Fail("key-b")
	current = current.Add(loginAttemptWindow + time.Minute)
	limiter.Allow("key-c")

	limiter.mu.Lock()
	_, retained := limiter.attempts["key-b"]
	limiter.mu.Unlock()
	if retained {
		t.Fatal("a stale limiter entry was not swept")
	}
}

func TestRequestIsHTTPSFromForwardedProto(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		want    bool
	}{
		{name: "no headers", want: false},
		{name: "forwarded https", headers: map[string]string{"X-Forwarded-Proto": "https"}, want: true},
		{name: "forwarded HTTP", headers: map[string]string{"X-Forwarded-Proto": "http"}, want: false},
		{name: "uppercase", headers: map[string]string{"X-Forwarded-Proto": "HTTPS"}, want: true},
		{name: "chain", headers: map[string]string{"X-Forwarded-Proto": "https, http"}, want: true},
		{name: "scheme header", headers: map[string]string{"X-Url-Scheme": "https"}, want: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/management/session", nil)
			for name, value := range testCase.headers {
				request.Header.Set(name, value)
			}
			if got := requestIsHTTPS(request); got != testCase.want {
				t.Fatalf("requestIsHTTPS() = %v, want %v", got, testCase.want)
			}
		})
	}
}

// Behind a TLS-terminating proxy the Secure attribute must still be set, or the
// browser drops the session cookie and sign-in silently fails.
func TestLoginSetsSecureCookieWhenForwardedProtoIsHTTPS(t *testing.T) {
	sessions := newFakeSessionStore("admin", "correct-horse-battery-staple")
	handler := (&Server{Sessions: sessions}).Handler()

	body := `{"username":"admin","password":"correct-horse-battery-staple"}`
	request := httptest.NewRequest(http.MethodPost, "/management/login", strings.NewReader(body))
	request.Header.Set("X-Forwarded-Proto", "https")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("login status = %d, want 200", response.Code)
	}
	if !sessionCookie(t, response).Secure {
		t.Fatal("session cookie is not Secure behind a TLS-terminating proxy")
	}
}

// The password must never reach the log, however the sign-in ended.
func TestLoginDoesNotLogCredentials(t *testing.T) {
	const password = "correct-horse-battery-staple"
	sessions := newFakeSessionStore("admin", password)
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	handler := (&Server{Sessions: sessions, Logger: logger}).Handler()

	submitLogin(t, handler, "admin", password)
	sessions.sessions = map[string]fakeSession{}
	submitLogin(t, handler, "admin", "wrong-password-entirely")

	logged := output.String()
	if strings.Contains(logged, password) {
		t.Fatalf("the log contains the password:\n%s", logged)
	}
	if strings.Contains(logged, "wrong-password-entirely") {
		t.Fatalf("the log contains a failed password:\n%s", logged)
	}
	if !strings.Contains(logged, "console_login_succeeded") {
		t.Errorf("expected a success audit event, log = %s", logged)
	}
	if !strings.Contains(logged, "console_login_failed") {
		t.Errorf("expected a failure audit event, log = %s", logged)
	}
}

// A malformed session store must fail closed rather than panic.
func TestSessionStoreErrorDoesNotPanic(t *testing.T) {
	handler := (&Server{Sessions: erroringSessionStore{}}).Handler()

	request := httptest.NewRequest(http.MethodPost, "/management/login", strings.NewReader(`{"username":"a","password":"b"}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", response.Code)
	}
	if strings.Contains(response.Body.String(), "boom") {
		t.Fatal("the internal error detail leaked to the client")
	}
}

type erroringSessionStore struct{}

func (erroringSessionStore) LoadAdminAccount(context.Context) (admin.Account, error) {
	return admin.Account{}, errors.New("boom")
}

func (erroringSessionStore) CreateAdminSession(context.Context, int64, string, time.Time, time.Time) error {
	return errors.New("boom")
}

func (erroringSessionStore) AuthenticateAdminSession(context.Context, string, time.Time) (admin.Account, error) {
	return admin.Account{}, errors.New("boom")
}

func (erroringSessionStore) DeleteAdminSession(context.Context, string) error {
	return errors.New("boom")
}
