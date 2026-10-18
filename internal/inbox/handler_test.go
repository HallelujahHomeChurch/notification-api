package inbox

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestInboxHTTPIdentityAndDisabledBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, path, caller, principal, user string
		status                              int
		duplicate                           bool
	}{
		{"anonymous", "/api/notifications/me", "", "", "", 401, false},
		{"non gateway", "/api/notifications/me", "operations-api", "user", actor, 403, false},
		{"service actor", "/api/notifications/me", "api-gateway", "service", actor, 403, false},
		{"duplicate caller", "/api/notifications/me", "api-gateway", "user", actor, 403, true},
		{"duplicate actor", "/api/notifications/me", "api-gateway", "user", actor, 401, true},
		{"gateway disabled", "/api/notifications/me", "api-gateway", "user", actor, 503, false},
		{"producer disabled", "/priv/notifications/inbox/batch", "operations-api", "", "", 503, false},
		{"email producer forbidden", "/priv/notifications/inbox/batch", "account-api", "", "", 403, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			method := "GET"
			if strings.HasPrefix(tc.path, "/priv/") {
				method = "POST"
			}
			r := httptest.NewRequest(method, tc.path, nil)
			if tc.caller != "" {
				r.Header.Set("Dapr-Caller-App-Id", tc.caller)
			}
			if tc.principal != "" {
				r.Header.Set("X-HHC-Principal-Type", tc.principal)
				r.Header.Set("X-HHC-Principal-ID", tc.user)
				r.Header.Set("X-HHC-User-ID", tc.user)
			}
			if tc.duplicate {
				if tc.name == "duplicate caller" {
					r.Header.Add("Dapr-Caller-App-Id", tc.caller)
				} else {
					r.Header.Add("X-HHC-User-ID", tc.user)
				}
			}
			w := httptest.NewRecorder()
			NewHandler(nil).ServeHTTP(w, r)
			if w.Code != tc.status || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("got %d/%q want %d/no-store", w.Code, w.Header().Get("Cache-Control"), tc.status)
			}
		})
	}
}
func TestInboxHTTPRejectsUnknownFieldsAndBindsRecipient(t *testing.T) {
	db := testDB(t)
	svc := NewService(NewStore(db), &policy{}, "test", keys)
	handler := NewHandler(svc)
	for _, tc := range []struct {
		path, body string
		status     int
	}{{"/priv/notifications/inbox/batch", `{"items":[],"producer":"engagement-api"}`, 422}, {"/priv/notifications/inbox/batch", `{"items":[]}`, 422}, {"/api/notifications/me/read-all", `{"snapshotToken":"bad","recipientAccountId":"other"}`, 422}, {"/api/notifications/me?limit=51", "", 422}, {"/api/notifications/me?filter=other", "", 422}, {"/api/notifications/me?limit=1&limit=2", "", 422}, {"/api/notifications/me?recipientAccountId=other", "", 422}} {
		method := http.MethodPost
		if strings.Contains(tc.path, "?") {
			method = http.MethodGet
		}
		r := httptest.NewRequest(method, tc.path, strings.NewReader(tc.body))
		r.Header.Set("Dapr-Caller-App-Id", "api-gateway")
		r.Header.Set("X-HHC-Principal-Type", "user")
		r.Header.Set("X-HHC-Principal-ID", actor)
		r.Header.Set("X-HHC-User-ID", actor)
		if strings.HasPrefix(tc.path, "/priv/") {
			r.Header.Set("Dapr-Caller-App-Id", "engagement-api")
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Errorf("%s = %d: %s", tc.path, w.Code, w.Body.String())
		}
	}
}

func TestBatchReturnsIndexedDurableAndExpiredOutcomes(t *testing.T) {
	db := testDB(t)
	handler := NewHandler(NewService(NewStore(db), &policy{}, "test", keys))
	now := time.Now().UTC()
	fresh := write("general", now)
	old := write("children", now.Add(-91*24*time.Hour))
	raw, _ := json.Marshal(struct {
		Items []Write `json:"items"`
	}{[]Write{fresh, fresh, old}})
	r := httptest.NewRequest("POST", "/priv/notifications/inbox/batch", bytes.NewReader(raw))
	r.Header.Set("Dapr-Caller-App-Id", "engagement-api")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	var result struct {
		Data struct {
			Items []struct {
				Index  int    `json:"index"`
				Status string `json:"status"`
				ID     string `json:"id"`
			} `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if w.Code != 202 || len(result.Data.Items) != 3 {
		t.Fatalf("missing outcomes %d %s", w.Code, w.Body.String())
	}
	for i, status := range []string{"accepted", "duplicate", "expired"} {
		if result.Data.Items[i].Index != i || result.Data.Items[i].Status != status {
			t.Fatalf("outcome %+v want index%d %s", result.Data.Items[i], i, status)
		}
	}
	if result.Data.Items[0].ID == "" || result.Data.Items[0].ID != result.Data.Items[1].ID {
		t.Fatal("duplicate identity changed")
	}
}
