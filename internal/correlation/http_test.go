package correlation

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCorrelationBoundary(t *testing.T) {
	const trace = "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"
	for _, tc := range []struct {
		name              string
		ids, traces       []string
		wantID, wantTrace string
	}{
		{"valid", []string{"request-1"}, []string{trace}, "request-1", trace},
		{"missing", nil, nil, "", ""},
		{"unsupported flags", nil, []string{trace[:53] + "02"}, "", ""},
		{"future version", nil, []string{"01" + trace[2:]}, "", ""},
		{"duplicate", []string{"first", "second"}, []string{trace, trace}, "", ""},
		{"comma", []string{"first,second"}, []string{trace + "," + trace}, "", ""},
		{"oversized", []string{strings.Repeat("a", 129)}, []string{trace + "extra"}, "", ""},
		{"space", []string{" private "}, []string{strings.ToUpper(trace)}, "", ""},
		{"zero trace", nil, []string{"00-" + strings.Repeat("0", 32) + "-0123456789abcdef-01"}, "", ""},
		{"zero span", nil, []string{"00-0123456789abcdef0123456789abcdef-" + strings.Repeat("0", 16) + "-01"}, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			req.Header["X-Hhc-Request-Id"] = tc.ids
			req.Header["Traceparent"] = tc.traces
			ctx, cancel := context.WithCancel(req.Context())
			cancel()
			req = req.WithContext(ctx)
			recorder := httptest.NewRecorder()
			Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				id := RequestID(r.Context())
				if !requestIDPattern.MatchString(id) || id != w.Header().Get("X-HHC-Request-ID") {
					t.Fatalf("inconsistent ID %q", id)
				}
				if tc.wantID != "" && id != tc.wantID {
					t.Fatal("valid ID replaced")
				}
				if tc.wantID == "" && len(id) != 32 {
					t.Fatal("missing random ID")
				}
				if Traceparent(r.Context()) != tc.wantTrace {
					t.Fatal("invalid trace handling")
				}
				if r.Context().Err() != context.Canceled {
					t.Fatal("lost cancellation")
				}
				if len(r.Header.Values("X-HHC-Request-ID")) != len(tc.ids) {
					t.Fatal("audit input rewritten")
				}
				w.WriteHeader(http.StatusServiceUnavailable)
			})).ServeHTTP(recorder, req)
			if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("X-HHC-Request-ID") == "" {
				t.Fatal("error response lost correlation")
			}
		})
	}
}
