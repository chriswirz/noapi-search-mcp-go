package main

import (
	"embed"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
)

// swaggerAssets is Swagger UI, compiled into the binary.
//
// Embedded rather than loaded from a CDN, and that is the point rather than a
// detail. This server is the sort of thing that runs on a build agent, inside
// a container with no egress, or on a laptop on a train - and documentation
// that only renders when the machine can reach cdnjs is documentation that is
// missing exactly when someone is trying to work out why nothing works. It
// costs about 1.6 MB of binary and it always works.
//
//go:embed static/swagger
var swaggerAssets embed.FS

// swaggerFS is the embedded directory with its path prefix stripped, so the
// files are served at the root of the docs subtree.
var swaggerFS = func() fs.FS {
	sub, err := fs.Sub(swaggerAssets, "static/swagger")
	if err != nil {
		// The directory is embedded at compile time, so this cannot fail in a
		// binary that built. Panicking says so, rather than serving a docs
		// page with no stylesheet and leaving someone to wonder why.
		panic("swagger assets are not embedded: " + err.Error())
	}
	return sub
}()

// handleDocs serves Swagger UI and its assets.
func (a *RESTAPI) handleDocs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD, OPTIONS")
		writeAPIError(w, http.StatusMethodNotAllowed, "", "the documentation takes GET")
		return
	}

	docs := a.cfg.API.DocsPath
	rest := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, docs), "/")
	if rest == "" || rest == "index.html" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.Method == http.MethodHead {
			return
		}
		if _, err := w.Write([]byte(a.docsHTML())); err != nil {
			a.logger.Printf("serving the documentation page: %v", err)
		}
		return
	}

	// Only the two asset files are served, by name. A file server rooted on
	// the embedded directory would do the same thing here, but naming them
	// keeps this subtree from ever becoming a general static host if something
	// else is embedded later.
	switch rest {
	case "swagger-ui.css":
		a.serveAsset(w, r, rest, "text/css; charset=utf-8")
	case "swagger-ui-bundle.js":
		a.serveAsset(w, r, rest, "application/javascript; charset=utf-8")
	default:
		http.NotFound(w, r)
	}
}

// serveAsset writes one embedded file.
func (a *RESTAPI) serveAsset(w http.ResponseWriter, r *http.Request, name, contentType string) {
	data, err := fs.ReadFile(swaggerFS, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType)
	// The assets are pinned to a build, so they can be cached hard. The spec
	// they render is not, and is served no-cache.
	w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
	if r.Method == http.MethodHead {
		return
	}
	if _, err := w.Write(data); err != nil {
		a.logger.Printf("serving %s: %v", name, err)
	}
}

// docsHTML is the Swagger UI page, pointed at this server's own spec.
//
// The token is not put into this page, and that is the point rather than an
// omission. An earlier version called preauthorizeApiKey with it and appended
// it to the spec URL, so that "Try it out" worked without anyone having to
// authorise. That was only ever safe while the page itself demanded the token
// to load - and demanding it was the bug, because a browser cannot send an
// Authorization header to a pasted URL. Serving the page openly and embedding
// the credential in it would hand the token to anyone who asked for the
// documentation.
//
// So the page carries no secret, and the reader authorises in the UI. The spec
// declares the bearer scheme, which is what puts the Authorize button there;
// persistAuthorization keeps what they enter across a reload, so it is asked
// for once rather than every time.
func (a *RESTAPI) docsHTML() string {
	docs := a.cfg.API.DocsPath
	spec := a.cfg.API.SpecPath

	// A note above the operations, so someone who has not noticed the button
	// is told before their first 401 rather than by it.
	authorise := ""
	if a.cfg.Server.AuthTokenValue() != "" {
		authorise = `
  <p class="auth-note">
    This server requires a bearer token. Choose <strong>Authorize</strong>
    and enter it before using <strong>Try it out</strong>.
  </p>`
	}

	return fmt.Sprintf(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>%s API</title>
  <link rel="stylesheet" href="%s/swagger-ui.css">
  <style>
    body { margin: 0; background: #fafafa; }
    .topbar { display: none; }
    .auth-note {
      margin: 0;
      padding: 12px 20px;
      background: #fff3cd;
      border-bottom: 1px solid #ffe69c;
      font: 14px/1.5 system-ui, sans-serif;
      color: #664d03;
    }
  </style>
</head>
<body>%s
  <div id="swagger-ui"></div>
  <script src="%s/swagger-ui-bundle.js"></script>
  <script>
    window.onload = function () {
      window.ui = SwaggerUIBundle({
        url: %s,
        dom_id: '#swagger-ui',
        deepLinking: true,
        displayRequestDuration: true,
        docExpansion: 'list',
        defaultModelsExpandDepth: 0,
        tryItOutEnabled: true,
        // Keeps the token entered through Authorize across a reload, so it is
        // asked for once rather than every time the page is opened.
        persistAuthorization: true,
        presets: [SwaggerUIBundle.presets.apis],
        layout: 'BaseLayout',
      });
    };
  </script>
</body>
</html>
`, htmlEscape(a.cfg.Server.Name), docs, authorise, docs, jsString(spec))
}

// jsString renders a Go string as a JavaScript literal. The values are a
// configured path and a configured token rather than anything a request
// carries, but they are still going into a script tag, and quoting them
// properly costs nothing.
func jsString(s string) string {
	// The angle brackets and ampersand are escaped as unicode rather than left
	// alone: this string is inside a <script> element, where a literal
	// "</script" would end the block whatever the JavaScript quoting says.
	replacer := strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		"\n", `\n`,
		"\r", `\r`,
		"<", `\u003c`,
		">", `\u003e`,
		"&", `\u0026`,
	)
	return `"` + replacer.Replace(s) + `"`
}

// htmlEscape escapes text going into the page title.
func htmlEscape(s string) string {
	return strings.NewReplacer(
		"&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;",
	).Replace(s)
}
