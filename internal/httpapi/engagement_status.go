package httpapi

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/HallelujahHomeChurch/notification-api/internal/correlation"
	"github.com/google/uuid"
)

const (
	engagementStatusPrefix      = "/priv/notifications/"
	engagementStatusTokenHeader = "X-HHC-Notification-Engagement-Token"
	engagementSendTokenHeader   = "X-HHC-Notification-Engagement-Send-Token"
	accountPrivateTokenHeader   = "X-HHC-Notification-Account-Token"
)

type PrivateCredentials struct {
	Account, EngagementSend, EngagementStatus string
}

func NewPrivateHandler(service notificationService, cleanup cleanupService, credentials PrivateCredentials) http.Handler {
	h := &handler{service: service, cleanup: cleanup}
	writesEnabled := credentials.Account != "" || credentials.EngagementSend != ""
	return correlation.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawPath != "" {
			writeError(w, r, http.StatusNotFound, "NTF_NOT_FOUND")
			return
		}
		var method string
		var handle http.HandlerFunc
		switch r.URL.Path {
		case "/priv/notifications/send":
			if writesEnabled {
				method, handle = http.MethodPost, h.send
			}
		case "/priv/account-cleanup":
			if writesEnabled {
				method, handle = http.MethodPost, h.cleanupAccount
			}
		default:
			messageID, ok := strings.CutPrefix(r.URL.Path, engagementStatusPrefix)
			id, err := uuid.Parse(messageID)
			if ok && err == nil && id.String() == messageID {
				r.SetPathValue("messageId", messageID)
				method, handle = http.MethodGet, h.get
			}
		}
		if handle == nil {
			writeError(w, r, http.StatusNotFound, "NTF_NOT_FOUND")
			return
		}
		if r.Method != method {
			writeError(w, r, http.StatusMethodNotAllowed, "NTF_METHOD_NOT_ALLOWED")
			return
		}
		caller := credentials.caller(r)
		if caller == "" {
			writeError(w, r, http.StatusForbidden, "NTF_FORBIDDEN")
			return
		}
		handle(w, r.WithContext(context.WithValue(r.Context(), callerKey, caller)))
	}))
}

func (c PrivateCredentials) caller(r *http.Request) string {
	for _, name := range []string{"Dapr-Caller-App-Id", "X-HHC-Caller-App-Id", "X-Internal-Caller-App-Id", "Dapr-Api-Token"} {
		if len(r.Header.Values(name)) != 0 {
			return ""
		}
	}

	var caller string
	for _, scope := range []struct {
		header, secret, caller string
		allowed                bool
	}{
		{accountPrivateTokenHeader, c.Account, "account-api", r.Method == http.MethodPost},
		{engagementSendTokenHeader, c.EngagementSend, "engagement-api", r.URL.Path == "/priv/notifications/send"},
		{engagementStatusTokenHeader, c.EngagementStatus, "engagement-api", r.Method == http.MethodGet},
	} {
		values := r.Header.Values(scope.header)
		if len(values) == 0 {
			continue
		}
		if caller != "" || !scope.allowed || scope.secret == "" || len(values) != 1 ||
			subtle.ConstantTimeCompare([]byte(values[0]), []byte(scope.secret)) != 1 {
			return ""
		}
		caller = scope.caller
	}
	return caller
}
