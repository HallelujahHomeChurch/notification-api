package notify

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
)

type Enqueuer interface {
	EnqueueEmail(context.Context, SendEmailRequest) error
}

func NewHandler(service Enqueuer, internalToken string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/priv/notification/v1/email", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
			return
		}
		if !validInternalToken(r, internalToken) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}

		var req SendEmailRequest
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
			return
		}
		if err := service.EnqueueEmail(r.Context(), req); err != nil {
			switch {
			case errors.Is(err, ErrInvalidRequest):
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
			case errors.Is(err, ErrRateLimited):
				writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate_limited"})
			case errors.Is(err, ErrDisabled):
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "notifications_disabled"})
			default:
				writeJSON(w, http.StatusBadGateway, map[string]string{"error": "enqueue_failed"})
			}
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued"})
	})
	return mux
}

func validInternalToken(r *http.Request, expected string) bool {
	if expected == "" {
		return true
	}
	got := r.Header.Get("X-Internal-Token")
	return subtle.ConstantTimeCompare([]byte(got), []byte(expected)) == 1
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
