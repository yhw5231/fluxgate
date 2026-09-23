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
		// The index is identified by a structural marker rather than by wording,
		// so a copy change does not read as a missing page.
		{name: "index", path: "/console/", wantType: "text/html", wantSnippet: `id="auth-overlay"`},
		{name: "stylesheet", path: "/console/styles.css", wantType: "text/css", wantSnippet: "--bg-base"},
		{name: "script", path: "/console/app.js", wantType: "text/javascript", wantSnippet: "management/snapshot"},
		{name: "theme script", path: "/console/theme.js", wantType: "text/javascript", wantSnippet: "data-theme"},
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

// Every text field must carry the shared field classes. The login inputs once
// relied on an id-based rule (a leftover #auth-token selector) that no longer
// matched them after the form changed, so they silently fell back to the
// browser default box. A class-based style cannot drift away like that, and
// this test fails if a field is added without it.
func TestEveryFieldInputCarriesTheSharedClasses(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/console/", nil)
	response := httptest.NewRecorder()
	Handler().ServeHTTP(response, request)

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	page := string(body)

	fields := []struct {
		id   string
		kind string
	}{
		{id: "auth-username", kind: "text"},
		{id: "auth-password", kind: "password"},
		{id: "current-password", kind: "password"},
		{id: "new-password", kind: "password"},
		{id: "confirm-password", kind: "password"},
		{id: "required-current-password", kind: "password"},
		{id: "required-new-password", kind: "password"},
		{id: "required-confirm-password", kind: "password"},
	}
	for _, field := range fields {
		tag := openingTagOf(t, page, `id="`+field.id+`"`)
		if !strings.Contains(tag, `class="field-input"`) {
			t.Errorf("input %s does not carry the shared field-input class: %s", field.id, tag)
		}
		if !strings.Contains(tag, `type="`+field.kind+`"`) {
			t.Errorf("input %s is not type %s: %s", field.id, field.kind, tag)
		}
	}

	// Each input needs a real label, which is also what keeps the field and its
	// caption in one block so the spacing cannot collapse.
	for _, id := range []string{
		"auth-username", "auth-password",
		"current-password", "new-password", "confirm-password",
		"required-current-password", "required-new-password", "required-confirm-password",
	} {
		if !strings.Contains(page, `for="`+id+`"`) {
			t.Errorf("input %s has no label bound to it", id)
		}
	}

	// Every field must sit inside the wrapper that supplies its spacing, or the
	// inputs run together with no gap.
	for _, field := range fields {
		if !hasAncestorIn(page, `id="`+field.id+`"`, `class="field"`) {
			t.Errorf("input %s is not wrapped in the shared field block", field.id)
		}
	}
}

// The stylesheet must not style a form input by an id that the page no longer
// has, which is how the login fields lost their box in the first place.
func TestStylesheetHasNoStaleAuthSelectors(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/console/styles.css", nil)
	response := httptest.NewRecorder()
	Handler().ServeHTTP(response, request)

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	stylesheet := string(body)

	// #auth-token was the single management-token input the login form used
	// before it collected a username and a password.
	if strings.Contains(stylesheet, "#auth-token") {
		t.Error("stylesheet still styles the removed #auth-token input")
	}
	// The shared rules the fields depend on have to exist, since removing them
	// while the classes stay in the markup is a silent regression.
	for _, wanted := range []string{".field-input", ".field-input:focus", ".field-label"} {
		if !strings.Contains(stylesheet, wanted) {
			t.Errorf("stylesheet is missing the shared rule %q", wanted)
		}
	}
}

// openingTagOf returns the markup of the opening tag containing marker.
func openingTagOf(t *testing.T, page, marker string) string {
	t.Helper()
	start := strings.Index(page, marker)
	if start == -1 {
		t.Fatalf("marker %q not found", marker)
	}
	open := strings.LastIndex(page[:start], "<")
	end := strings.Index(page[start:], ">")
	if open == -1 || end == -1 {
		t.Fatalf("marker %q is not inside an opening tag", marker)
	}
	return page[open : start+end]
}

// hasAncestorIn reports whether an opening tag for ancestor appears before
// marker and is still open at that point. The console markup keeps each field
// in its own block, so the nearest preceding ancestor tag is enough.
func hasAncestorIn(page, marker, ancestor string) bool {
	start := strings.Index(page, marker)
	if start == -1 {
		return false
	}
	ancestorStart := strings.LastIndex(page[:start], ancestor)
	if ancestorStart == -1 {
		return false
	}
	// The ancestor must not have been closed again before the marker.
	return strings.LastIndex(page[:start], "</div>") < ancestorStart
}

// A credential must not be persisted by the console itself: the session is an
// HTTP-only cookie, so no token may be written to web storage. The one thing the
// console does keep there is the day/night choice, and that lives in theme.js.
func TestConsoleDoesNotPersistCredentialsInWebStorage(t *testing.T) {
	script := consoleScript(t)

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
		`data-view="settings"`,
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

// The settings view is grouped by function: the account, the runtime policy and
// the proxies are separate panels, and the bar inside the view switches between
// them, so no single page carries all three at once.
func TestConsoleGroupsSettingsByFunction(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/console/", nil)
	response := httptest.NewRecorder()
	Handler().ServeHTTP(response, request)

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	page := string(body)

	if !strings.Contains(page, `id="settings-nav"`) {
		t.Error("console settings view has no sub navigation")
	}
	for _, group := range []string{"account", "policy", "proxies"} {
		for _, wanted := range []string{
			`data-settings-panel="` + group + `"`,
			`data-settings-view="` + group + `"`,
		} {
			if !strings.Contains(page, wanted) {
				t.Errorf("console settings view is missing %q", wanted)
			}
		}
	}
	// Only the first group is open before the script runs, so the others have to
	// carry the hidden attribute in the markup rather than relying on the script.
	for _, group := range []string{"policy", "proxies"} {
		if !strings.Contains(page, `data-settings-panel="`+group+`" hidden`) {
			t.Errorf("console settings group %q is not closed in the markup", group)
		}
	}
	if strings.Contains(page, `data-settings-panel="account" hidden`) {
		t.Error("console settings opens on a group the script has to reveal")
	}
}

// The settings sub navigation is switched by the script, so the script has to
// know the groups and carry the function that shows one.
func TestConsoleScriptSwitchesSettingsGroups(t *testing.T) {
	script := consoleScript(t)

	for _, wanted := range []string{
		"SETTINGS_VIEWS",
		"showSettingsView(",
		"'[data-settings-panel]'",
		"'settings-nav'",
	} {
		if !strings.Contains(script, wanted) {
			t.Errorf("console script is missing %q", wanted)
		}
	}
}

// Adding and editing configuration is the console's second job, so the page has
// to carry a view and a create button per managed resource, and the dialog the
// editor form renders into.
func TestConsoleCarriesTheManagementViewsAndEditor(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/console/", nil)
	response := httptest.NewRecorder()
	Handler().ServeHTTP(response, request)

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	page := string(body)

	// The upstream is the one form the gateway configures as a whole; the client
	// keys and the proxies are still plain tables, and the routing table is
	// derived rather than written, so it carries no create button.
	for _, resource := range []string{"upstreams", "proxies", "keys"} {
		for _, wanted := range []string{
			`data-management="` + resource + `"`,
			`data-create="` + resource + `"`,
		} {
			if !strings.Contains(page, wanted) {
				t.Errorf("console is missing %q for %s", wanted, resource)
			}
		}
	}
	for _, wanted := range []string{
		`data-table="routing"`,
		`data-body="routing"`,
		`id="policy-panels"`,
		`id="breakers-reset-all"`,
		// 请求记录页：记录表、筛选和清空都要在脚本跑起来之前就存在。
		`data-table="requests"`,
		`data-requests-body`,
		`data-empty="requests"`,
		`id="requests-failed"`,
		`id="requests-model"`,
		`id="requests-clear"`,
	} {
		if !strings.Contains(page, wanted) {
			t.Errorf("console is missing %q", wanted)
		}
	}
	// Each view toggles as a whole, so the container and its navigation entry
	// have to exist even before the script runs.
	for _, view := range []string{"overview", "upstream", "routes", "requests", "keys", "settings"} {
		if !strings.Contains(page, `data-view="`+view+`"`) {
			t.Errorf("console navigation has no %q entry", view)
		}
	}
	for _, wanted := range []string{
		`id="editor-overlay"`,
		`id="editor-form"`,
		`id="editor-fields"`,
		`id="confirm-overlay"`,
		`id="secret-overlay"`,
	} {
		if !strings.Contains(page, wanted) {
			t.Errorf("console is missing %q", wanted)
		}
	}
}

// The management script must write through the same origin-relative paths it
// reads, and it must never fall back to storing a credential in the browser.
func TestConsoleScriptWritesConfigurationThroughTheApi(t *testing.T) {
	script := consoleScript(t)

	// Writes reach the API through the relative helper, so a mounted prefix
	// keeps working.
	for _, wanted := range []string{"apiPath(", "'/management/configuration'", "'/management/configuration/keys/'"} {
		if !strings.Contains(script, wanted) {
			t.Errorf("console script is missing %q", wanted)
		}
	}
	// A stored secret arrives as a mask and the field itself submits empty, so
	// the only credential the script ever holds is one it just created — plus one
	// client key it read back on the operator's explicit request.
	for _, wanted := range []string{"openSecret(", "留空保持不变", "当前值："} {
		if !strings.Contains(script, wanted) {
			t.Errorf("console script is missing %q", wanted)
		}
	}
	for _, wanted := range []string{"'/reveal'", "revealClientKey(", "copyClientKey(", "writeClipboard("} {
		if !strings.Contains(script, wanted) {
			t.Errorf("console script is missing %q", wanted)
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
	script := consoleScript(t)

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

// consoleScript reads the management script, which is where the form behaviour
// under test lives.
func consoleScript(t *testing.T) string {
	t.Helper()
	return consoleAsset(t, "/console/app.js")
}

// consoleAsset reads one of the files the console serves.
func consoleAsset(t *testing.T, path string) string {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	response := httptest.NewRecorder()
	Handler().ServeHTTP(response, request)
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

// The console carries two colour schemes, and which one it opens with has to be
// decided before the first paint: a script that ran with the app would draw the
// other scheme first and then flip. That is why the theme script is a file of
// its own, loaded from the head, and why the switch it binds lives in the top
// bar. The light palette is a second set of values for the same variables, so
// no rule needs a second body.
func TestConsoleSwitchesBetweenDayAndNight(t *testing.T) {
	page := consoleAsset(t, "/console/")

	themeTag := strings.Index(page, `src="theme.js"`)
	if themeTag == -1 {
		t.Fatal("the console page does not load the theme script")
	}
	if headEnd := strings.Index(page, "</head>"); headEnd == -1 || themeTag > headEnd {
		t.Error("the theme script is not loaded from the head, so the first paint is unthemed")
	}
	if !strings.Contains(page, `id="theme-button"`) {
		t.Error("the top bar carries no day/night switch")
	}

	script := consoleAsset(t, "/console/theme.js")
	for _, wanted := range []string{
		"getElementById('theme-button')",
		"'fluxgate.theme'",
		"prefers-color-scheme",
		"data-theme",
		"localStorage",
	} {
		if !strings.Contains(script, wanted) {
			t.Errorf("the theme script is missing %q", wanted)
		}
	}

	stylesheet := consoleAsset(t, "/console/styles.css")
	for _, wanted := range []string{
		`:root[data-theme="light"]`,
		"color-scheme: light",
	} {
		if !strings.Contains(stylesheet, wanted) {
			t.Errorf("the stylesheet is missing %q", wanted)
		}
	}
}

// A credential field is not a place a browser may fill in. The console's own
// login password is the credential saved for this origin, so a password manager
// would put it into the key editor and the value would be stored as a key.
func TestCredentialFieldsRefuseToBeAutofilled(t *testing.T) {
	script := consoleScript(t)

	for _, wanted := range []string{
		`data-bwignore`,
		`data-1p-ignore`,
		`data-lpignore`,
		`data-form-type`,
		`autocomplete = 'new-password'`,
		// The field is read-only until it is focused and anything that appears
		// without a keystroke is cleared, which is what actually stops the
		// managers that ignore the attributes.
		"readOnly",
		"keepUnfilled(",
	} {
		if !strings.Contains(script, wanted) {
			t.Errorf("the console script is missing %q", wanted)
		}
	}
}

// 排除模型、限定路由、限定上游、排除上游 are chosen from a list the console already
// holds: a restriction typed as JSON by hand is how a policy ends up naming a route
// or a model that does not exist.
func TestKeyRestrictionsArePickedFromFetchedLists(t *testing.T) {
	script := consoleScript(t)

	for _, field := range []string{"supported_models", "allowed_route_ids", "allowed_site_ids", "excluded_site_ids"} {
		if !strings.Contains(script, field+": {") {
			t.Errorf("the console declares no field metadata for %s", field)
		}
	}
	for _, wanted := range []string{
		"createChoiceList",
		"type: 'choice_list'",
		"candidates: modelCandidates",
		"candidates: routeCandidates",
		"candidates: upstreamCandidates",
		// The lists come from the configuration the console has read.
		"function resourceRows(",
		"state.configuration.models",
		"不在当前列表中",
	} {
		if !strings.Contains(script, wanted) {
			t.Errorf("the console script is missing %q", wanted)
		}
	}
	// The JSON textarea renderer the id lists used to have is gone: leaving it in
	// place would let a field fall back to typing ids by hand.
	if strings.Contains(script, "类型: 'id_list'") || strings.Contains(script, "type === 'id_list'") {
		t.Error("the console still renders an id list as raw JSON")
	}
}

// An upstream address is written the way the operator thinks of it, and the form
// makes one canonical address of the spellings that mean the same upstream.
func TestUpstreamAddressIsCompletedInTheForm(t *testing.T) {
	script := consoleScript(t)

	for _, wanted := range []string{
		"function normalizeAPIAddress(",
		"normalize: normalizeAPIAddress",
		"ENDPOINT_SUFFIX",
		// The probe asks with the normalized address and the proxy from the form,
		// so it goes out the way a real request would.
		"proxy_url: editorFieldValue('proxy_url')",
		"url: url",
	} {
		if !strings.Contains(script, wanted) {
			t.Errorf("the console script is missing %q", wanted)
		}
	}
}

// A create shows the value the gateway would store for an omitted field, so an
// upstream added in the console is enabled rather than left unset.
func TestConsoleShowsTheServerDefaultInTheForm(t *testing.T) {
	script := consoleScript(t)

	for _, wanted := range []string{
		"field.default_value",
		"!row && field.default_value",
	} {
		if !strings.Contains(script, wanted) {
			t.Errorf("the console script is missing %q", wanted)
		}
	}
}
