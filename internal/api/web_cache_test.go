package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAHandlersOwnCachePolicyNeverReachesTheNetworkFlushedOrUnwritten(t *testing.T) {
	inner := http.NewServeMux()
	inner.HandleFunc("GET /v1/flusher", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		w.(http.Flusher).Flush()
		io.WriteString(w, "after the flush")
	})
	inner.HandleFunc("GET /v1/controller", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Error(err)
		}
		io.WriteString(w, "after the flush")
	})
	inner.HandleFunc("GET /v1/quiet", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
	})
	server := httptest.NewServer(withRequestID(&web{network: inner, host: "home.adanext.si", origin: testOrigin}))
	t.Cleanup(server.Close)
	for _, path := range []string{"/v1/flusher", "/v1/controller", "/v1/quiet"} {
		req, _ := http.NewRequest("GET", server.URL+path, nil)
		req.Host = "home.adanext.si"
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 || resp.Header.Values("Cache-Control")[0] != "no-store" || len(resp.Header.Values("Cache-Control")) != 1 {
			t.Fatalf("%s: %d Cache-Control %q", path, resp.StatusCode, resp.Header.Values("Cache-Control"))
		}
	}
}
