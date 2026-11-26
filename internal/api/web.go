package api

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/cristiangirlea/ada-os/internal/store"
)

// contentSecurityPolicy is the policy of the shared UI's page, ui/index.html, sent as a header so
// that frame-ancestors applies.
const contentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'; object-src 'none'"

// WebConfig is the TLS listener's channel: Name, the configured host name; Origin,
// https://name[:port] as ListenerOrigin makes it; Owner, the owner account; UI, the built shared
// UI, nil serving no static assets. Clock nil means time.Now.
type WebConfig struct {
	Name   string
	Origin string
	Owner  string
	UI     fs.FS
	Clock  func() time.Time
}

type web struct {
	network http.Handler
	host    string // the Host a request must carry: the origin without its scheme
	origin  string
	ui      fs.FS
}

// NewWeb serves the network channel behind the browser boundary: the configured Host only,
// mutations only from the configured origin as JSON, no cross-origin headers, the shared UI's
// policy on every response, and the shared UI's files without a session.
func NewWeb(s *store.Store, hub *Hub, cfg WebConfig, execution ...Execution) http.Handler {
	n, err := newNetwork(s, hub, NetworkConfig{RPID: cfg.Name, Origin: cfg.Origin, Owner: cfg.Owner, Clock: cfg.Clock}, execution...)
	if err != nil {
		panic(fmt.Sprintf("api: web channel: %v", err))
	}
	return withRequestID(newWeb(n, cfg))
}

func newWeb(n *network, cfg WebConfig) http.Handler {
	return &web{network: n, host: strings.ToLower(strings.TrimPrefix(cfg.Origin, "https://")), origin: cfg.Origin, ui: cfg.UI}
}

func (wb *web) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	header := w.Header()
	header.Set("Content-Security-Policy", contentSecurityPolicy)
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Referrer-Policy", "no-referrer")
	api := r.URL.Path == "/v1" || strings.HasPrefix(r.URL.Path, "/v1/")
	if api {
		w = &noStoreWriter{ResponseWriter: w}
		header.Set("Cache-Control", "no-store")
	}
	// Only origin-form: an absolute-form target would carry its own host past the check below.
	if !strings.HasPrefix(r.RequestURI, "/") || r.URL.IsAbs() {
		writeError(w, 400, "bad_request")
		return
	}
	// An exact match also refuses DNS rebinding: another name never reaches a route.
	if strings.ToLower(r.Host) != wb.host {
		writeError(w, http.StatusMisdirectedRequest, "host_invalid")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		if origins := r.Header.Values("Origin"); len(origins) != 1 || origins[0] != wb.origin {
			writeError(w, 403, "origin_refused")
			return
		}
		types := r.Header.Values("Content-Type")
		if len(types) != 1 {
			writeError(w, http.StatusUnsupportedMediaType, "content_type_refused")
			return
		}
		if media, _, err := mime.ParseMediaType(types[0]); err != nil || media != "application/json" {
			writeError(w, http.StatusUnsupportedMediaType, "content_type_refused")
			return
		}
	}
	if api {
		wb.network.ServeHTTP(w, r)
		// A handler that set its own policy and wrote nothing is answered by the server after this.
		header.Set("Cache-Control", "no-store")
		return
	}
	wb.static(w, r)
}

// static serves the shared UI's files: index.html for /, never a directory listing and never a
// name outside the UI.
func (wb *web) static(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "" {
		name = "index.html"
	}
	if wb.ui == nil || !fs.ValidPath(name) {
		writeError(w, 404, "not_found")
		return
	}
	if !wb.serveFile(w, r, name) {
		writeError(w, 404, "not_found")
	}
}

// uiTypes are the content types of the files a UI build ships, fixed here because on Windows
// mime.TypeByExtension takes the machine's registry over Go's own table.
var uiTypes = map[string]string{
	".html":  "text/html; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".mjs":   "text/javascript; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".json":  "application/json",
	".map":   "application/json",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".ico":   "image/x-icon",
	".woff":  "font/woff",
	".woff2": "font/woff2",
	".txt":   "text/plain; charset=utf-8",
}

// serveFile answers name when it is a regular file of the UI, and reports whether it did.
func (wb *web) serveFile(w http.ResponseWriter, r *http.Request, name string) bool {
	f, err := wb.ui.Open(name)
	if err != nil {
		return false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	content, ok := f.(io.ReadSeeker)
	if !ok {
		data, err := io.ReadAll(f)
		if err != nil {
			return false
		}
		content = bytes.NewReader(data)
	}
	if ct, ok := uiTypes[strings.ToLower(path.Ext(name))]; ok {
		w.Header().Set("Content-Type", ct)
	}
	http.ServeContent(w, r, name, info.ModTime(), content)
	return true
}

// noStoreWriter keeps Cache-Control at no-store whatever a handler set before writing or flushing.
type noStoreWriter struct {
	http.ResponseWriter
}

func (w *noStoreWriter) WriteHeader(status int) {
	w.Header().Set("Cache-Control", "no-store")
	w.ResponseWriter.WriteHeader(status)
}

func (w *noStoreWriter) Write(p []byte) (int, error) {
	w.Header().Set("Cache-Control", "no-store")
	return w.ResponseWriter.Write(p)
}

func (w *noStoreWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *noStoreWriter) Flush() {
	w.Header().Set("Cache-Control", "no-store")
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func (w *noStoreWriter) FlushError() error {
	w.Header().Set("Cache-Control", "no-store")
	return http.NewResponseController(w.ResponseWriter).Flush()
}
