// Package correlation carries bounded HTTP diagnostics, never audit provenance.
package correlation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"regexp"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

type key struct{}
type values struct{ requestID, traceparent string }

var spanRequestIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
var traceparentPattern = regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-0[01]$`)

// Middleware leaves supplied headers untouched for independent audit validation.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var v values
		ids := r.Header.Values("X-HHC-Request-ID")
		if len(ids) == 1 && requestIDPattern.MatchString(ids[0]) {
			v.requestID = ids[0]
		}
		if v.requestID == "" {
			var b [16]byte
			if _, err := rand.Read(b[:]); err != nil {
				http.Error(w, "request id unavailable", http.StatusInternalServerError)
				return
			}
			v.requestID = hex.EncodeToString(b[:])
		}
		traces := r.Header.Values("traceparent")
		if len(traces) == 1 && traceparentPattern.MatchString(traces[0]) && traces[0][3:35] != strings.Repeat("0", 32) && traces[0][36:52] != strings.Repeat("0", 16) {
			v.traceparent = traces[0]
		}
		if spanRequestIDPattern.MatchString(v.requestID) {
			trace.SpanFromContext(r.Context()).SetAttributes(attribute.String("hhc.request_id", v.requestID))
		}
		w.Header().Set("X-HHC-Request-ID", v.requestID)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), key{}, v)))
	})
}

func RequestID(ctx context.Context) string   { v, _ := ctx.Value(key{}).(values); return v.requestID }
func Traceparent(ctx context.Context) string { v, _ := ctx.Value(key{}).(values); return v.traceparent }
