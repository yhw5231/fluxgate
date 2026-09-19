// Package console serves the embedded management console UI for the gateway.
// The console is static, unauthenticated HTML; every data request it makes
// goes through the token-protected management endpoints.
package console

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed static
var staticFiles embed.FS

const consoleRoot = "/console"

// Handler returns an http.Handler serving the console under /console. The
// handler is intentionally self-contained so tests and alternative servers
// can mount it without the full gateway runtime.
func Handler() http.Handler {
	root, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic(err) // unreachable: the static directory is embedded at build time
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		name := strings.TrimPrefix(r.URL.Path, consoleRoot)
		switch name {
		case "", "/":
			name = "index.html"
		default:
			name = strings.TrimPrefix(name, "/")
		}

		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy",
			"default-src 'none'; style-src 'self'; script-src 'self'; connect-src 'self'; "+
				"img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		http.ServeFileFS(w, r, root, name)
	})
}
