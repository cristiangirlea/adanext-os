package api

import (
	"net/http/httptest"
	"testing"
)

func TestARepeatedOriginOrContentTypeIsRefusedEvenWhenEachCopyIsRight(t *testing.T) {
	f := newNetFixture(t)
	h := webHandler(f, portOrigin, testUI)
	for _, c := range []struct {
		origins, types []string
		status         int
		code           string
	}{
		{[]string{portOrigin, portOrigin}, []string{"application/json"}, 403, "origin_refused"},
		{[]string{portOrigin}, []string{"application/json", "application/json"}, 415, "content_type_refused"},
	} {
		req := httptest.NewRequest("POST", "/v1/session/passkey/options", nil)
		req.Host = "home.adanext.si:8443"
		req.Header["Origin"] = c.origins
		req.Header["Content-Type"] = c.types
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if r := readReply(t, rec.Result()); r.status != c.status || r.code(t) != c.code {
			t.Fatalf("Origin %q, Content-Type %q: %d %s", c.origins, c.types, r.status, r.body)
		}
	}
}
