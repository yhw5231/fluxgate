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
// the client fingerprint catalog are separate panels, and the bar inside the view
// switches between them, so no single page carries all three at once. The proxies
// are a view of their own rather than a settings panel, because they are part of
// what clients are given (访问) rather than of the process.
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
	for _, group := range []string{"account", "policy", "profiles"} {
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
	for _, group := range []string{"policy", "profiles"} {
		if !strings.Contains(page, `data-settings-panel="`+group+`" hidden`) {
			t.Errorf("console settings group %q is not closed in the markup", group)
		}
	}
	if strings.Contains(page, `data-settings-panel="account" hidden`) {
		t.Error("console settings opens on a group the script has to reveal")
	}

	// The proxies left the settings view for one of their own.
	if !strings.Contains(page, `data-view-panel="proxies"`) || !strings.Contains(page, `data-view="proxies"`) {
		t.Error("console has no proxies view of its own")
	}
	if strings.Contains(page, `data-settings-view="proxies"`) {
		t.Error("console still treats the proxies as a settings group")
	}
}

// The navigation is grouped by what each view is for — 监控, 渠道, 访问, 系统 —
// and every view names its group in one place, so a page is placeable without
// reading the whole sidebar. The top bar names the group and view on screen.
func TestConsoleGroupsNavigationByFunction(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/console/", nil)
	response := httptest.NewRecorder()
	Handler().ServeHTTP(response, request)

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	page := string(body)

	for _, wanted := range []string{
		`class="console-sidebar"`,
		`class="console-main"`,
		`id="topbar-crumb"`,
		`nav-group-title">监控`,
		`nav-group-title">渠道`,
		`nav-group-title">访问`,
		`nav-group-title">系统`,
	} {
		if !strings.Contains(page, wanted) {
			t.Errorf("console navigation is missing %q", wanted)
		}
	}
	// Every view is reachable from the sidebar, and every view has a panel, so the
	// script's toggle has something to show for each name it accepts.
	for _, view := range []string{
		"overview", "usage", "probes", "requests",
		"upstream", "routes", "pins",
		"keys", "proxies", "pools",
		"settings",
	} {
		for _, wanted := range []string{
			`data-view="` + view + `"`,
			`data-view-panel="` + view + `"`,
		} {
			if !strings.Contains(page, wanted) {
				t.Errorf("console has no %q for the %s view", wanted, view)
			}
		}
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

// The script's list of views is what decides whether a navigation click shows a
// panel: a view that exists in the markup but not in that list silently falls back
// to the overview. The two have to name the same set.
func TestConsoleScriptAcceptsEveryViewItCarries(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/console/", nil)
	response := httptest.NewRecorder()
	Handler().ServeHTTP(response, request)

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	page := string(body)
	script := consoleScript(t)

	start := strings.Index(script, "var VIEWS = [")
	if start == -1 {
		t.Fatal("console script has no VIEWS list")
	}
	end := strings.Index(script[start:], "]")
	if end == -1 {
		t.Fatal("console script's VIEWS list is not closed")
	}
	views := script[start : start+end]

	seen := map[string]bool{}
	marker := `data-view="`
	for index := 0; index < len(page); {
		next := strings.Index(page[index:], marker)
		if next == -1 {
			break
		}
		from := index + next + len(marker)
		closing := strings.Index(page[from:], `"`)
		if closing == -1 {
			break
		}
		name := page[from : from+closing]
		if name != "" && !strings.Contains(name, " ") {
			seen[name] = true
		}
		index = from + closing
	}
	if len(seen) == 0 {
		t.Fatal("the console markup carries no views")
	}
	for name := range seen {
		if !strings.Contains(views, "'"+name+"'") {
			t.Errorf("the %q view exists in the markup but not in the script's VIEWS list", name)
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
	for _, resource := range []string{"upstreams", "proxies", "keys", "pools"} {
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
	for _, view := range []string{"overview", "usage", "probes", "upstream", "routes", "pins", "requests", "keys", "proxies", "pools", "settings"} {
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

// 提示浮在视口角落，不占据文档流：以前它顶在页面上面，出现时会把整页内容往下
// 推，而操作者正在读的就是被推走的那一屏。固定定位就是这条性质本身，所以这里
// 断言样式，而不是某个具体坐标。提示也自己消失——它报告的是已经发生的事，没有
// 需要手动关掉的对话框，否则一个每十秒刷新一次的页面会把同样的失败提示反复弹
// 出来。
func TestNoticesFloatAndDismissThemselves(t *testing.T) {
	page := consoleAsset(t, "/console/")
	if strings.Contains(page, "banner-holder") {
		t.Error("提示又被放回了文档流里的容器，出现时会把页面内容推下去")
	}
	if !strings.Contains(page, `class="toast-stack"`) {
		t.Fatal("页面里没有提示容器")
	}
	for _, unwanted := range []string{`id="banner-dismiss"`, `id="notice-dismiss"`} {
		if strings.Contains(page, unwanted) {
			t.Errorf("提示上还挂着手动关闭的按钮 %q，它应该自己消失", unwanted)
		}
	}

	rule := cssRule(consoleAsset(t, "/console/styles.css"), ".toast-stack")
	if !strings.Contains(rule, "position: fixed") {
		t.Error("提示容器不是固定定位，出现时仍会把页面内容推下去")
	}
	// 模态框里的保存失败也走这条提示，所以要压在遮罩（z-index 40）上面。
	if !strings.Contains(rule, "z-index: 50") {
		t.Error("提示容器的层级不够，会被模态框的遮罩盖住")
	}

	script := consoleScript(t)
	for _, wanted := range []string{
		"autoDismiss(els.banner",
		"autoDismiss(els.notice",
		"function refreshFailure(",
	} {
		if !strings.Contains(script, wanted) {
			t.Errorf("the console script is missing %q", wanted)
		}
	}
}

// cssRule returns the body of the first rule declared with exactly this selector,
// which is enough to read a property out of a single rule.
func cssRule(stylesheet, selector string) string {
	start := strings.Index(stylesheet, selector+" {")
	if start == -1 {
		return ""
	}
	rest := stylesheet[start:]
	open := strings.Index(rest, "{")
	end := strings.Index(rest, "}")
	if open == -1 || end < open {
		return ""
	}
	return rest[open:end]
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

// A channel renames itself upstream, so asking for the model list again has to
// rebuild it from the answer: an entry the upstream now lists under another name
// is re-pointed at that name instead of keeping the one it was picked under, a
// model the upstream has just gained is left unchecked — a selection is a route,
// and a name that merely appeared is not the operator asking for it — and one it
// no longer lists is kept, marked and deselected rather than dropped or left
// routed to. 全选 and 反选 are there for picking a whole listing at once, and they
// act on the rows the filter shows.
func TestConsoleRebuildsTheModelListOnEveryFetch(t *testing.T) {
	script := consoleScript(t)

	for _, wanted := range []string{
		"function applyListing(",
		"function refreshEntry(",
		"var listing = applyListing(models)",
		// 上游现在的写法覆盖旧的，除非「上游模型名」是手写的另一个模型。
		"canonicalModelName(entry.target) === entry.name",
		// 上游新增的模型按未勾选处理，上游不再提供的取消勾选并标出。
		"addModel(spelling, false, '')",
		"entry.checked = false",
		"（上游未列出）",
		// 一次勾完一个渠道的模型，不必逐个点。
		"'全选'",
		"'反选'",
		"visibleRows().forEach",
	} {
		if !strings.Contains(script, wanted) {
			t.Errorf("console script is missing %q", wanted)
		}
	}
}

// A platform that serves its models from channel groups lists them as
// "cn:<model>", so the console has to file such a model under the name after the
// colon and carry the group's spelling as the name to send: the group names the
// line that serves the model, and the operator picks a model rather than one of a
// platform's groups.
func TestConsoleReadsAChannelGroupAsTheLine(t *testing.T) {
	script := consoleScript(t)

	for _, wanted := range []string{
		// 第一个冒号前是渠道分组，冒号后面才是模型。
		"var group = name.indexOf(':')",
		"name = name.slice(group + 1)",
		// 表单里说清楚模型名与「上游模型名」各填什么。
		"cn:deepseek-v4.1-flash",
		"「上游模型名」自动填",
		// 同一个模型的另一种分组写法留在行里，operator 才知道还有别的可选。
		"entry.spellings",
		"（上游另有 ",
		// 记录条数与模型个数不同时说清楚是按分组归并的。
		"按渠道分组归并后是 ",
	} {
		if !strings.Contains(script, wanted) {
			t.Errorf("console script is missing %q", wanted)
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

// 启用与停用是列表页上最常做的一个动作，所以状态徽标本身就是开关：点它切换，
// 走的是只带 status 的局部更新，密钥、模型和映射都不必重填一遍。
func TestConsoleTogglesUpstreamStatusFromTheList(t *testing.T) {
	script := consoleScript(t)

	for _, wanted := range []string{
		// 状态列渲染的是可点的按钮，而不是只读徽标。
		"cell: function (row) { return upstreamStatusToggle(row); }",
		"function upstreamStatusToggle(",
		"function toggleUpstreamStatus(",
		"badge badge-toggle ",
		// 一次点击只写 status 一个字段，其余字段保持已保存的值。
		"'/management/configuration/upstreams/' + row.id",
		"{ status: next }",
		// 切换之后两张表都重画，并在提示里说清现在的状态。
		"renderRouting();",
		"已停用「",
		"已启用「",
	} {
		if !strings.Contains(script, wanted) {
			t.Errorf("the console script is missing %q", wanted)
		}
	}
	// 只读的状态徽标仍被其它资源使用，但上游的状态列不能再退回去用它。
	if strings.Contains(script, "{ label: '状态', cell: function (row) { return statusBadge(row.status); } }") {
		t.Error("the upstream status column still renders a read-only badge")
	}
}

// 每个密钥的并发是上游级的设置：表单里填一个数字，列表里只有真的设置过才标出来，
// 默认不限。填 1 表示这个上游的每个密钥同一时间只处理一个请求，其余请求排队等待。
func TestConsoleCarriesThePerKeyConcurrencySetting(t *testing.T) {
	script := consoleScript(t)

	for _, wanted := range []string{
		"key_concurrency: {",
		"label: '密钥并发'",
		"同一时间只处理一个请求",
		"function upstreamKeyConcurrency(",
		"function keyConcurrencyLabel(",
		// 没设置过的上游不占标签，设置过的才显示上限。
		"if (upstreamKeyConcurrency(row) > 0) {",
		// 展开的行说清楚这个上游的密钥现在能同时处理几个请求。
		"每个密钥同时最多处理 ",
	} {
		if !strings.Contains(script, wanted) {
			t.Errorf("console script is missing %q", wanted)
		}
	}
}

// 概览里的熔断记录按线路或按密钥归档。按线路归档的用它自己的 id 恢复；按密钥归档的
// 记录里没有密钥（密钥只在网关里），所以按它所属的上游恢复——上游是浏览器能指到的、
// 又覆盖这条 circuit 的最小单位，同一个密钥串挂在两个上游时也不会误清另一个。
func TestConsoleRecoversACircuitItCannotName(t *testing.T) {
	script := consoleScript(t)

	for _, wanted := range []string{
		"function breakerResetTarget(",
		"scope: 'channel', channel_id: entry.channel_id",
		"scope: 'site', site_id: number(entry.site_id)",
		"post('/management/breakers/reset', target)",
	} {
		if !strings.Contains(script, wanted) {
			t.Errorf("console script is missing %q", wanted)
		}
	}
}

// A pin names providers the upstream has never been asked about, so the console
// carries two actions that ask it: a discovery (which providers exist, which
// pipeline is spoken) and a validation (does each provider answer now).
func TestConsoleCarriesThePinProbeActions(t *testing.T) {
	script := consoleScript(t)
	for _, wanted := range []string{
		"/management/pins/probe",
		"/management/pins/validate",
		"probePinRow",
		"validatePinRow",
		"probePinRow(row, probe)",
		"validatePinRow(row, validate)",
	} {
		if !strings.Contains(script, wanted) {
			t.Errorf("console script is missing %q", wanted)
		}
	}
}

// 探测页先回答「现在能核对哪些线路、每条线路问哪个模型」：这张表读的是网关的当前
// 配置，记录表读的是历史结果。两张表都在同一页，操作者不用去别处拼出「这条线路到底
// 能不能探测」。
func TestConsoleCarriesTheProbeTargets(t *testing.T) {
	page := consoleAsset(t, "/console/")
	for _, wanted := range []string{
		// 表格、表体和空状态都要在脚本跑起来之前就存在。
		`data-probes-targets-table`,
		`data-probes-targets-body`,
		`data-probes-targets-empty`,
		// 表头说清这一列是「探测模型」，以及线路清单从哪里来。
		`<th>探测模型</th>`,
	} {
		if !strings.Contains(page, wanted) {
			t.Errorf("console page is missing %q", wanted)
		}
	}

	script := consoleScript(t)
	for _, wanted := range []string{
		"renderProbeTargets",
		"state.probes.targets",
		// 同一个上游的几个密钥是几条不同的线路，名字一样，只有密钥标签分得开。
		"lineKeyLabel(target.channel_id)",
		// 冷却中的线路照样列出来，只是带一个徽标：手动探测正是用它确认恢复。
		"target.held",
		"冷却中",
		// 没有结果和「上游没回答」是两件事：前者说还没有探测，后者是 0。
		"还没有探测",
	} {
		if !strings.Contains(script, wanted) {
			t.Errorf("console script is missing %q", wanted)
		}
	}
}

// 请求记录里既有客户端请求，也有网关自己的账号探测；错误要能展开读全文，也要能
// 整段复制出来——复制是排查时唯一不必手抄表单的动作。
func TestConsoleRequestRecordsLabelProbesAndCopyContent(t *testing.T) {
	script := consoleScript(t)
	for _, wanted := range []string{
		// 探测记录带标签，不会被当成客户端请求。
		"record.source === 'probe'",
		"requestResultCell",
		"这是网关自己的账号探测",
		// 展开后的详情整段可复制，单个上游返回也能单独复制。
		"copyRequestRecord",
		"复制详情",
		"copyCellButton",
		"writeClipboard(lines.join",
		// 探测记录没有客户端密钥，不能显示成 #0。
		"账号探测",
	} {
		if !strings.Contains(script, wanted) {
			t.Errorf("console script is missing %q", wanted)
		}
	}
}
