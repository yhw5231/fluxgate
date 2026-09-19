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
		{name: "script", path: "/console/app.js", wantType: "text/javascript", wantSnippet: "/management/snapshot"},
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
