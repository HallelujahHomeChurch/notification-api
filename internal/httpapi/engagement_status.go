package httpapi

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/HallelujahHomeChurch/notification-api/internal/correlation"
	"github.com/google/uuid"
)

const engagementStatusPrefix = "/priv/notifications/"
const engagementStatusTokenHeader = "X-HHC-Notification-Engagement-Token"

func NewEngagementStatusHandler(service notificationService, token string) http.Handler {
	h := &handler{service: service}
	return correlation.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		messageID, ok := strings.CutPrefix(r.URL.Path, engagementStatusPrefix)
		id, err := uuid.Parse(messageID)
		if !ok || err != nil || id.String() != messageID || r.URL.RawPath != "" {
			writeError(w, r, http.StatusNotFound, "NTF_NOT_FOUND")
			return
		}
		if r.Method != http.MethodGet {
			writeError(w, r, http.StatusMethodNotAllowed, "NTF_METHOD_NOT_ALLOWED")
			return
		}
		for _, name := range []string{"Dapr-Caller-App-Id", "X-HHC-Caller-App-Id", "X-Internal-Caller-App-Id", "Dapr-Api-Token"} {
			if len(r.Header.Values(name)) != 0 {
				writeError(w, r, http.StatusForbidden, "NTF_FORBIDDEN")
				return
			}
		}
		credentials := r.Header.Values(engagementStatusTokenHeader)
		if token == "" || len(credentials) != 1 || subtle.ConstantTimeCompare([]byte(credentials[0]), []byte(token)) != 1 {
			writeError(w, r, http.StatusForbidden, "NTF_FORBIDDEN")
			return
		}
		r.SetPathValue("messageId", messageID)
		h.get(w, r.WithContext(context.WithValue(r.Context(), callerKey, "engagement-api")))
	}))
}
