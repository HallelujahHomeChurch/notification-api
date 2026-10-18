package inbox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestVisibilityWirePreservesSeriesAndUnwrapsOwnerEnvelopes(t *testing.T) {
	for _, enveloped := range []bool{false, true} {
		t.Run(map[bool]string{false: "canonical", true: "owner envelope"}[enveloped], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req VisibilityRequest
				if json.NewDecoder(r.Body).Decode(&req) != nil || req.ActorID != actor || req.Locale != "zh-Hant" || len(req.Items) != 2 {
					t.Error("invalid private request")
				}
				result := VisibilityResponse{Items: []VisibilityItem{}}
				for _, item := range req.Items {
					v := VisibilityItem{ID: item.ID, Visible: item.Resource.Series == "general"}
					if v.Visible {
						v.Projection = &Projection{TitleKey: "bulletin", MessageKey: "published", Action: &Action{Type: "bulletin", Resource: item.Resource, Locale: "en"}}
					}
					result.Items = append(result.Items, v)
				}
				if enveloped {
					json.NewEncoder(w).Encode(map[string]any{"data": result, "meta": map[string]any{}, "error": nil})
				} else {
					json.NewEncoder(w).Encode(result)
				}
			}))
			defer server.Close()
			client := NewVisibilityClient("", server.URL, "")
			projections, err := client.Check(context.Background(), actor, "zh-Hant", []Ref{{ID: actor, Resource: Resource{Type: "bulletin", ID: "issue", Series: "general"}}, {ID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Resource: Resource{Type: "bulletin", ID: "issue", Series: "children"}}})
			if err != nil || len(projections) != 1 || projections[actor].Action.Resource.Series != "general" {
				t.Fatalf("result %+v %v", projections, err)
			}
		})
	}
}
func TestVisibilityRejectsPartialMalformedOrUnsafeOwnerResponse(t *testing.T) {
	for _, raw := range []string{`{"data":{"items":[{"id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","visible":false}]},"error":{"code":"unavailable"}}`, `{"items":[{"id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}]}`, `{"items":[]}`, `{"items":[{"id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","visible":true}]}`, `{"items":[{"id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","visible":true,"projection":{"titleKey":"recording","messageKey":"published","action":{"type":"url","url":"javascript:alert(1)"}}}]}`, `{"data":null}`, `{"items":[{"id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","visible":false,"projection":{"titleKey":"leaked","messageKey":"name"}}]}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(raw)) }))
		_, err := NewVisibilityClient("", server.URL, "").Check(context.Background(), actor, "zh-Hant", []Ref{{ID: actor, Resource: Resource{Type: "recording", ID: "id"}}})
		server.Close()
		if err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
}
