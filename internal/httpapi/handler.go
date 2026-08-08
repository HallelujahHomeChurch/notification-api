package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/HallelujahHomeChurch/notification-api/internal/contracts"
	"github.com/HallelujahHomeChurch/notification-api/internal/service"
	"github.com/google/uuid"
)

const maxRequestBody = 64 << 10

type notificationService interface {
	Send(context.Context, string, string, contracts.SendRequest) (service.Result, error)
	Get(context.Context, string, string) (service.Result, error)
}

type pinger interface {
	PingContext(context.Context) error
}

type handler struct {
	service              notificationService
	db                   pinger
	allowedCallers       map[string]struct{}
	allowDevCallerHeader bool
}

type contextKey int

const (
	requestIDKey contextKey = iota
	callerKey
)

type responseEnvelope struct {
	Data  any                      `json:"data"`
	Meta  contracts.ResponseMeta   `json:"meta"`
	Error *contracts.ResponseError `json:"error"`
}

func New(service notificationService, db pinger, allowedCallers []string, allowDevCallerHeader bool) http.Handler {
	h := &handler{
		service:              service,
		db:                   db,
		allowedCallers:       make(map[string]struct{}, len(allowedCallers)),
		allowDevCallerHeader: allowDevCallerHeader,
	}
	for _, caller := range allowedCallers {
		if caller = strings.TrimSpace(caller); caller != "" {
			h.allowedCallers[caller] = struct{}{}
		}
	}

	mux := http.NewServeMux()
	mux.Handle("/health", h.requireMethod(http.MethodGet, http.HandlerFunc(h.health)))
	mux.Handle("/ready", h.requireMethod(http.MethodGet, http.HandlerFunc(h.ready)))
	mux.Handle("/priv/notifications/send", h.requireMethod(http.MethodPost, h.authorize(http.HandlerFunc(h.send))))
	mux.Handle("/priv/notifications/{messageId}", h.requireMethod(http.MethodGet, h.authorize(http.HandlerFunc(h.get))))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, http.StatusNotFound, "NTF_NOT_FOUND")
	})
	return h.withRequestID(mux)
}

func (h *handler) withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := strings.TrimSpace(r.Header.Get("X-HHC-Request-ID"))
		if requestID == "" {
			requestID = uuid.NewString()
		}
		w.Header().Set("X-HHC-Request-ID", requestID)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, requestID)))
	})
}

func (h *handler) authorize(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller := strings.TrimSpace(r.Header.Get("Dapr-Caller-App-Id"))
		if caller == "" && h.allowDevCallerHeader {
			caller = strings.TrimSpace(r.Header.Get("X-HHC-Caller-App-Id"))
		}
		if caller == "" {
			writeError(w, r, http.StatusUnauthorized, "NTF_UNAUTHORIZED")
			return
		}
		if _, ok := h.allowedCallers[caller]; !ok {
			writeError(w, r, http.StatusForbidden, "NTF_FORBIDDEN")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), callerKey, caller)))
	})
}

func (h *handler) requireMethod(method string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			writeError(w, r, http.StatusMethodNotAllowed, "NTF_METHOD_NOT_ALLOWED")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *handler) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "healthy"})
}

func (h *handler) ready(w http.ResponseWriter, r *http.Request) {
	if h.db == nil {
		writeError(w, r, http.StatusServiceUnavailable, "NTF_NOT_READY")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := h.db.PingContext(ctx); err != nil {
		writeError(w, r, http.StatusServiceUnavailable, "NTF_NOT_READY")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (h *handler) send(w http.ResponseWriter, r *http.Request) {
	idempotencyKey := r.Header.Get("Idempotency-Key")
	if idempotencyKey == "" {
		writeError(w, r, http.StatusBadRequest, "NTF_INVALID_REQUEST")
		return
	}

	var request contracts.SendRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	result, err := h.service.Send(r.Context(), callerFromContext(r.Context()), idempotencyKey, request)
	if err != nil {
		handleServiceError(w, r, err)
		return
	}
	writeEnvelope(w, http.StatusAccepted, r, contracts.SendResponseData{
		MessageID:       result.MessageID,
		Status:          result.Status,
		TemplateVersion: result.TemplateVersion,
		Replayed:        result.Replayed,
	}, nil)
}

func (h *handler) get(w http.ResponseWriter, r *http.Request) {
	result, err := h.service.Get(r.Context(), callerFromContext(r.Context()), r.PathValue("messageId"))
	if err != nil {
		handleServiceError(w, r, err)
		return
	}
	data := map[string]any{
		"messageId": result.MessageID,
		"status":    result.Status,
	}
	if result.FailureCode != "" {
		data["failureCode"] = result.FailureCode
	}
	writeEnvelope(w, http.StatusOK, r, data, nil)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, destination any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		writeError(w, r, http.StatusBadRequest, "NTF_INVALID_REQUEST")
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, r, http.StatusBadRequest, "NTF_INVALID_REQUEST")
		return false
	}
	return true
}

func handleServiceError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, service.ErrInvalidRequest):
		writeError(w, r, http.StatusBadRequest, "NTF_INVALID_REQUEST")
	case errors.Is(err, service.ErrForbiddenTemplate):
		writeError(w, r, http.StatusForbidden, "NTF_FORBIDDEN")
	case errors.Is(err, service.ErrIdempotencyConflict):
		writeError(w, r, http.StatusConflict, "NTF_IDEMPOTENCY_CONFLICT")
	case errors.Is(err, service.ErrRateLimited):
		w.Header().Set("Retry-After", "1")
		var rateLimitError *service.RateLimitError
		if errors.As(err, &rateLimitError) {
			w.Header().Set("Retry-After", retryAfterSeconds(rateLimitError.RetryAfter))
		}
		writeError(w, r, http.StatusTooManyRequests, "NTF_RATE_LIMITED")
	case errors.Is(err, service.ErrNotFound):
		writeError(w, r, http.StatusNotFound, "NTF_NOT_FOUND")
	case errors.Is(err, service.ErrNotificationsDisabled):
		writeError(w, r, http.StatusServiceUnavailable, "NTF_DISABLED")
	default:
		writeError(w, r, http.StatusInternalServerError, "NTF_INTERNAL")
	}
}

func retryAfterSeconds(duration time.Duration) string {
	seconds := duration / time.Second
	if duration%time.Second != 0 {
		seconds++
	}
	if seconds < 1 {
		seconds = 1
	}
	return strconv.FormatInt(int64(seconds), 10)
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code string) {
	writeEnvelope(w, status, r, nil, &contracts.ResponseError{Code: code})
}

func writeEnvelope(w http.ResponseWriter, status int, r *http.Request, data any, responseError *contracts.ResponseError) {
	writeJSON(w, status, responseEnvelope{
		Data:  data,
		Meta:  contracts.ResponseMeta{RequestID: requestIDFromContext(r.Context())},
		Error: responseError,
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func callerFromContext(ctx context.Context) string {
	caller, _ := ctx.Value(callerKey).(string)
	return caller
}

func requestIDFromContext(ctx context.Context) string {
	requestID, _ := ctx.Value(requestIDKey).(string)
	return requestID
}
