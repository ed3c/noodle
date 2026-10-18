package server

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReviewSelectionOnlyAllowsSameOriginExplicitJSON(t *testing.T) {
	h := LoopViewsHandler(t.TempDir(), nil, nil)
	for _, test := range []struct {
		origin, content, body string
		status                int
	}{
		{"", "application/json", "{}", 403},
		{"http://elsewhere", "application/json", "{}", 403},
		{"http://example.com", "text/plain", "{}", 403},
		{"http://example.com", "application/json", `{"command":"run"}`, 400},
		{"http://example.com", "application/json", "{} {}", 400},
	} {
		r := httptest.NewRequest("POST", "http://example.com/api/loops/review-selections", strings.NewReader(test.body))
		r.Header.Set("Origin", test.origin)
		r.Header.Set("Content-Type", test.content)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != test.status {
			t.Fatalf("expected %d got %d", test.status, w.Code)
		}
	}
}
