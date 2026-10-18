package inbox

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/google/uuid"
)

// NewHandler uses Dapr-authenticated caller identity only; development proxy headers are not accepted.
func NewHandler(service *Service) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		requestID := r.Header.Get("X-HHC-Request-ID")
		if requestID == "" {
			requestID = uuid.NewString()
		}
		w.Header().Set("X-HHC-Request-ID", requestID)
		respond := func(status int, data any, code string) {
			var fault any
			if code != "" {
				fault = map[string]string{"code": code}
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			json.NewEncoder(w).Encode(map[string]any{"data": data, "meta": map[string]string{"requestId": requestID}, "error": fault})
		}
		fail := func(err error) {
			switch {
			case errors.Is(err, ErrInvalid):
				respond(422, nil, "NTF_INVALID_REQUEST")
			case errors.Is(err, ErrConflict):
				respond(409, nil, "NTF_IDEMPOTENCY_CONFLICT")
			case errors.Is(err, ErrNotFound):
				respond(404, nil, "NTF_NOT_FOUND")
			default:
				respond(503, nil, "NTF_INBOX_UNAVAILABLE")
			}
		}
		callers := r.Header.Values("Dapr-Caller-App-Id")
		if len(callers) == 0 {
			respond(401, nil, "NTF_UNAUTHORIZED")
			return
		}
		if len(callers) != 1 {
			respond(403, nil, "NTF_FORBIDDEN")
			return
		}
		caller := callers[0]
		private := r.URL.Path == "/priv/notifications/inbox/batch"
		actor := ""
		if private {
			if caller != "operations-api" && caller != "engagement-api" {
				respond(403, nil, "NTF_FORBIDDEN")
				return
			}
		} else {
			if caller != "api-gateway" {
				respond(403, nil, "NTF_FORBIDDEN")
				return
			}
			principal := r.Header.Values("X-HHC-Principal-Type")
			if len(principal) != 1 || principal[0] != "user" {
				respond(403, nil, "NTF_FORBIDDEN")
				return
			}
			users := r.Header.Values("X-HHC-User-ID")
			ids := r.Header.Values("X-HHC-Principal-ID")
			if len(users) != 1 || !validActor(users[0]) || len(ids) != 1 || ids[0] != users[0] {
				respond(401, nil, "NTF_UNAUTHORIZED")
				return
			}
			actor = users[0]
		}
		decode := func(v any) bool {
			d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
			d.DisallowUnknownFields()
			if d.Decode(v) != nil || d.Decode(&struct{}{}) != io.EOF {
				fail(ErrInvalid)
				return false
			}
			return true
		}
		expected := http.MethodGet
		if private || r.URL.Path == "/api/notifications/me/read-all" || r.PathValue("id") != "" {
			expected = http.MethodPost
		}
		if r.Method != expected {
			respond(405, nil, "NTF_METHOD_NOT_ALLOWED")
			return
		}
		if service == nil {
			fail(ErrUnavailable)
			return
		}
		if r.URL.Path != "/api/notifications/me" && r.URL.Path != "/api/notifications/me/summary" && r.URL.RawQuery != "" {
			fail(ErrInvalid)
			return
		}
		if private {
			var request struct {
				Items []Write `json:"items"`
			}
			if !decode(&request) {
				return
			}
			result, err := service.AcceptBatch(r.Context(), caller, request.Items)
			if err != nil {
				fail(err)
				return
			}
			respond(202, result, "")
			return
		}
		switch r.URL.Path {
		case "/api/notifications/me":
			q := r.URL.Query()
			for key, values := range q {
				if len(values) != 1 || (key != "filter" && key != "cursor" && key != "limit" && key != "locale") {
					fail(ErrInvalid)
					return
				}
			}
			filter := q.Get("filter")
			if filter == "" {
				filter = "all"
			}
			limit := 20
			if raw, ok := q["limit"]; ok {
				n, err := strconv.Atoi(raw[0])
				if err != nil {
					fail(ErrInvalid)
					return
				}
				limit = n
			}
			page, err := service.Page(r.Context(), actor, filter, q.Get("cursor"), limit, displayLocale(r))
			if err != nil {
				fail(err)
				return
			}
			respond(200, page, "")
		case "/api/notifications/me/summary":
			if len(r.URL.Query()) > 1 || len(r.URL.Query()["locale"]) > 1 || (len(r.URL.Query()) == 1 && r.URL.Query().Get("locale") == "") {
				fail(ErrInvalid)
				return
			}
			summary, err := service.Summary(r.Context(), actor, displayLocale(r))
			if err != nil {
				fail(err)
				return
			}
			respond(200, summary, "")
		case "/api/notifications/me/read-all":
			var request struct {
				SnapshotToken string `json:"snapshotToken"`
			}
			if !decode(&request) {
				return
			}
			if err := service.MarkAllRead(r.Context(), actor, request.SnapshotToken); err != nil {
				fail(err)
				return
			}
			respond(200, map[string]bool{"read": true}, "")
		default:
			id := r.PathValue("id")
			if id == "" {
				respond(404, nil, "NTF_NOT_FOUND")
				return
			}
			if err := service.MarkRead(r.Context(), actor, id); err != nil {
				fail(err)
				return
			}
			respond(200, map[string]bool{"read": true}, "")
		}
	})
}

func displayLocale(r *http.Request) string {
	locale := r.URL.Query().Get("locale")
	if !r.URL.Query().Has("locale") {
		return "zh-Hant"
	}
	return locale
}
