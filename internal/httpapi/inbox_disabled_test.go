package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// This catches missing inbox routes and accidental enablement during rollout.
func TestInboxDisabledIsUnavailableAndNoStore(t *testing.T) {
	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/notifications/me"}, {"GET", "/api/notifications/me/summary"},
		{"POST", "/api/notifications/me/read-all"}, {"POST", "/api/notifications/me/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa/read"},
		{"POST", "/priv/notifications/inbox/batch"},
	} {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		r.Header.Set("Dapr-Caller-App-Id", "api-gateway")
		r.Header.Set("X-HHC-Principal-Type", "user")
		r.Header.Set("X-HHC-Principal-ID", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
		r.Header.Set("X-HHC-User-ID", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
		if tc.path == "/priv/notifications/inbox/batch" {
			r.Header.Set("Dapr-Caller-App-Id", "operations-api")
		}
		w := httptest.NewRecorder()
		New(nil, nil, nil, false).ServeHTTP(w, r)
		if w.Code != http.StatusServiceUnavailable || w.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s %s = %d, cache %q; want 503/no-store", tc.method, tc.path, w.Code, w.Header().Get("Cache-Control"))
		}
	}
}
