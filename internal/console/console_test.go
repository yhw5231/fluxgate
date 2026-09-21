package console

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerServesIndexAndAssets(t *testing.T) {
	handler := Handler()

	cases := []struct {
		name        string
		path        string
		wantType    string
		wantSnippet string
	}{
		{name: "index", path: "/console/", wantType: "text/html", wantSnippet: "Fluxgate Console"},
		{name: "stylesheet", path: "/console/styles.css", wantType: "text/css", wantSnippet: "--bg-base"},
		{name: "script", path: "/console/app.js", wantType: "text/javascript", wantSnippet: "management/snapshot"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, testCase.path, nil)
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", response.Code)
			}
			if got := response.Header().Get("Content-Type"); !strings.Contains(got, testCase.wantType) {
				t.Fatalf("Content-Type = %q, want it to contain %q", got, testCase.wantType)
			}
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if !strings.Contains(string(body), testCase.wantSnippet) {
				t.Fatalf("body does not contain %q", testCase.wantSnippet)
			}
		})
	}
}

func TestHandlerSendsHardeningHeaders(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/console/", nil)
	response := httptest.NewRecorder()

	Handler().ServeHTTP(response, request)

	expected := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
		"Cache-Control":          "no-store",
	}
	for header, want := range expected {
		if got := response.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}

	policy := response.Header().Get("Content-Security-Policy")
	if policy == "" {
		t.Fatal("Content-Security-Policy is missing")
	}
	for _, directive := range []string{"default-src 'none'", "script-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(policy, directive) {
			t.Errorf("Content-Security-Policy = %q, want it to contain %q", policy, directive)
		}
	}
}

func TestHandlerRejectsWriteMethods(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/console/", strings.NewReader("{}"))
	response := httptest.NewRecorder()

	Handler().ServeHTTP(response, request)

	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", response.Code)
	}
	if got := response.Header().Get("Allow"); got != "GET, HEAD" {
		t.Fatalf("Allow = %q, want %q", got, "GET, HEAD")
	}
}

func TestHandlerReturnsNotFoundForUnknownAsset(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/console/missing.js", nil)
	response := httptest.NewRecorder()

	Handler().ServeHTTP(response, request)

	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.Code)
	}
}

func TestHandlerBlocksPathTraversal(t *testing.T) {
	paths := []string{
		"/console/../console.go",
		"/console/../../go.mod",
		"/console/%2e%2e/go.mod",
		"/console/..%2fgo.mod",
	}
	for _, path := range paths {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		response := httptest.NewRecorder()

		Handler().ServeHTTP(response, request)

		if response.Code == http.StatusOK {
			t.Errorf("%s returned 200; traversal was not blocked", path)
		}
	}
}

// The console has to keep working when a reverse proxy mounts it under a path
// prefix, so no asset may be referenced from the server root and the management
// calls must be resolved against the page URL at runtime.
func TestConsoleReferencesEverythingRelatively(t *testing.T) {
	cases := []struct {
		assetPath string
		unwanted  []string
		wanted    []string
	}{
		{
			assetPath: "/console/",
			unwanted:  []string{`href="/console/`, `src="/console/`},
			wanted:    []string{`href="styles.css"`, `src="app.js"`},
		},
		{
			assetPath: "/console/app.js",
			unwanted:  []string{`fetch('/management`, `fetch("/management`},
			wanted:    []string{"apiPath(", "lastIndexOf('/console/')"},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.assetPath, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, testCase.assetPath, nil)
			response := httptest.NewRecorder()

			Handler().ServeHTTP(response, request)

			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			for _, unwanted := range testCase.unwanted {
				if strings.Contains(string(body), unwanted) {
					t.Errorf("%s contains root-absolute reference %q", testCase.assetPath, unwanted)
				}
			}
			for _, wanted := range testCase.wanted {
				if !strings.Contains(string(body), wanted) {
					t.Errorf("%s is missing %q", testCase.assetPath, wanted)
				}
			}
		})
	}
}

// The console signs in with an account, not a pasted token, so the form must
// collect both fields and must not carry a token input any more.
func TestConsoleLoginFormCollectsUsernameAndPassword(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/console/", nil)
	response := httptest.NewRecorder()
	Handler().ServeHTTP(response, request)

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	page := string(body)

	for _, wanted := range []string{
		`id="auth-username"`,
		`id="auth-password"`,
		`autocomplete="username"`,
		`autocomplete="current-password"`,
	} {
		if !strings.Contains(page, wanted) {
			t.Errorf("login form is missing %q", wanted)
		}
	}
	if strings.Contains(page, "auth-token") {
		t.Error("login form still has a management token input")
	}
}

// A credential must not be persisted by the console itself: the session is an
// HTTP-only cookie, so no token may be written to web storage.
func TestConsoleDoesNotPersistCredentialsInWebStorage(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/console/app.js", nil)
	response := httptest.NewRecorder()
	Handler().ServeHTTP(response, request)

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	script := string(body)

	for _, unwanted := range []string{"sessionStorage", "localStorage"} {
		if strings.Contains(script, unwanted) {
			t.Errorf("console script uses %s to persist credentials", unwanted)
		}
	}
	if !strings.Contains(script, "credentials: 'same-origin'") {
		t.Error("console requests do not send the session cookie")
	}
}

// The settings page is a second view inside the signed-in app, reachable from
// the top bar, and it carries the password form.
func TestConsoleHasSettingsViewWithPasswordForm(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/console/", nil)
	response := httptest.NewRecorder()
	Handler().ServeHTTP(response, request)

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	page := string(body)

	for _, wanted := range []string{
		`id="settings-button"`,
		`id="settings-view"`,
		`id="password-form"`,
		`id="current-password"`,
		`id="new-password"`,
		`id="confirm-password"`,
		`id="password-submit"`,
	} {
		if !strings.Contains(page, wanted) {
			t.Errorf("console is missing %q", wanted)
		}
	}
}

// A password manager filling the change form would rotate the stored credential
// without the operator noticing, so the form must opt out explicitly.
func TestPasswordFormsDisableAutofill(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/console/", nil)
	response := httptest.NewRecorder()
	Handler().ServeHTTP(response, request)

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	page := string(body)

	for _, formID := range []string{"password-form", "required-password-form"} {
		marker := `id="` + formID + `"`
		start := strings.Index(page, marker)
		if start == -1 {
			t.Fatalf("form %q not found", formID)
		}
		// Inspect up to the end of the opening tag.
		openEnd := strings.Index(page[start:], ">")
		if openEnd == -1 {
			t.Fatalf("form %q has no opening tag end", formID)
		}
		opening := page[start : start+openEnd]
		if !strings.Contains(opening, `autocomplete="off"`) {
			t.Errorf("form %q does not set autocomplete=\"off\": %s", formID, opening)
		}
		// A fixed action keeps a password manager from treating it as a login
		// form it should save.
		if !strings.Contains(opening, `action="about:blank"`) {
			t.Errorf("form %q does not pin its action: %s", formID, opening)
		}
	}

	// Every password input must declare a new-password autocomplete token and the
	// vendor opt-outs, which is what actually suppresses the common managers.
	for _, id := range []string{
		`id="current-password"`, `id="new-password"`, `id="confirm-password"`,
		`id="required-current-password"`, `id="required-new-password"`, `id="required-confirm-password"`,
	} {
		start := strings.Index(page, id)
		if start == -1 {
			t.Fatalf("input %q not found", id)
		}
		end := strings.Index(page[start:], ">")
		tag := page[start : start+end]
		for _, attribute := range []string{
			`autocomplete="new-password"`, `data-1p-ignore`, `data-lpignore="true"`, `data-bwignore="true"`,
		} {
			if !strings.Contains(tag, attribute) {
				t.Errorf("input %s is missing %s", id, attribute)
			}
		}
	}
}

// The forced-change screen is what an operator sees on a fresh deployment.
func TestConsoleHasForcedPasswordChangeScreen(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/console/", nil)
	response := httptest.NewRecorder()
	Handler().ServeHTTP(response, request)

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	page := string(body)

	for _, wanted := range []string{
		`id="password-required"`,
		`id="required-password-form"`,
		`id="required-current-password"`,
		`id="required-new-password"`,
		`id="required-confirm-password"`,
	} {
		if !strings.Contains(page, wanted) {
			t.Errorf("console is missing %q", wanted)
		}
	}
}

// The console must send the password change to the documented endpoint.
func TestConsolePostsPasswordChange(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/console/app.js", nil)
	response := httptest.NewRecorder()
	Handler().ServeHTTP(response, request)

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	script := string(body)

	for _, wanted := range []string{
		"management/password",
		"current_password",
		"new_password",
		"change_required",
	} {
		if !strings.Contains(script, wanted) {
			t.Errorf("console script is missing %q", wanted)
		}
	}
}
