package studio

import (
	"embed"
	"io/fs"
	"net/http"
)

// bundle holds the built Studio web UI. The dist directory is produced by the
// separate lebro-studio project and copied in at release time; the .gitkeep
// keeps the directory present so this embed always compiles, and a build that
// has not copied a bundle in serves the placeholder page below rather than
// failing to compile.
//
//go:embed all:dist
var bundle embed.FS

// assetHandler serves the embedded UI bundle at the root, falling back to a
// placeholder page when no real bundle is embedded. When a bundle is present,
// an unknown path serves index.html so the single-page app's client-side router
// can resolve it, rather than 404ing on a deep link.
func (s *Studio) assetHandler() http.Handler {
	dist, err := fs.Sub(bundle, "dist")
	if err != nil {
		return http.HandlerFunc(placeholderPage)
	}
	index, err := fs.ReadFile(dist, "index.html")
	if err != nil {
		// No bundle copied in: serve the placeholder so the API is still
		// reachable and the operator learns why the UI is blank.
		return http.HandlerFunc(placeholderPage)
	}

	fileServer := http.FileServer(http.FS(dist))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Serve a real file when one exists; otherwise hand the app shell to the
		// client router. HEAD is treated like GET by http.FileServer already.
		if _, statErr := fs.Stat(dist, assetPath(r.URL.Path)); statErr == nil {
			fileServer.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(index)
	})
}

// assetPath maps a request path to the fs.FS key it would be served from. The
// root maps to index.html; every other path drops its leading slash, because
// io/fs keys are unrooted.
func assetPath(urlPath string) string {
	if urlPath == "/" || urlPath == "" {
		return "index.html"
	}
	return urlPath[1:]
}

// placeholderPage explains that no UI bundle is embedded and that the API is
// still available. It is what a from-source build serves until a bundle is
// copied into dist.
func placeholderPage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(placeholderHTML))
}

const placeholderHTML = `<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><title>lebro studio</title></head>
<body style="font-family:system-ui,sans-serif;max-width:40rem;margin:4rem auto;padding:0 1rem;line-height:1.5">
<h1>lebro studio</h1>
<p>No UI bundle is embedded in this build. The developer UI lives in the
<code>lebro-studio</code> project; build it and copy its output into
<code>studio/dist</code> to serve it here.</p>
<p>The API is running. Try <code>GET /api/agents</code>,
<code>GET /api/workflows</code>, or <code>GET /api/studio/traces</code>.</p>
</body>
</html>
`
