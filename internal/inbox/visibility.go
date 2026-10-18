package inbox

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

type VisibilityRequest struct {
	ActorID string `json:"actorId"`
	Locale  string `json:"locale"`
	Items   []Ref  `json:"items"`
}
type VisibilityItem struct {
	ID         string      `json:"id"`
	Visible    bool        `json:"visible"`
	Projection *Projection `json:"projection,omitempty"`
}
type VisibilityResponse struct {
	Items []VisibilityItem `json:"items"`
}
type visibilityClient struct {
	operationsURL, websiteURL, daprToken string
	http                                 *http.Client
}

func NewVisibilityClient(operationsURL, websiteURL, daprToken string) VisibilityChecker {
	return &visibilityClient{operationsURL, websiteURL, daprToken, &http.Client{Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (c *visibilityClient) Check(ctx context.Context, actor, locale string, refs []Ref) (map[string]Projection, error) {
	if !validLocale(locale) || !validActor(actor) || len(refs) > 100 {
		return nil, ErrInvalid
	}
	out := map[string]Projection{}
	for _, group := range []struct {
		endpoint string
		org      bool
	}{{c.operationsURL, true}, {c.websiteURL, false}} {
		selected := []Ref{}
		known := map[string]Resource{}
		for _, ref := range refs {
			if !validActor(ref.ID) || !ref.Resource.valid() {
				return nil, ErrInvalid
			}
			if (ref.Resource.Type == "org_join_request") == group.org {
				if _, dup := known[ref.ID]; dup {
					return nil, ErrInvalid
				}
				selected = append(selected, ref)
				known[ref.ID] = ref.Resource
			}
		}
		if len(selected) == 0 {
			continue
		}
		if group.endpoint == "" {
			return nil, ErrUnavailable
		}
		body, _ := json.Marshal(VisibilityRequest{actor, locale, selected})
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, group.endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, ErrUnavailable
		}
		request.Header.Set("Content-Type", "application/json")
		if c.daprToken != "" {
			request.Header.Set("dapr-api-token", c.daprToken)
		}
		response, err := c.http.Do(request)
		if err != nil {
			return nil, ErrUnavailable
		}
		raw, readErr := io.ReadAll(io.LimitReader(response.Body, (256<<10)+1))
		response.Body.Close()
		if response.StatusCode != http.StatusOK || readErr != nil || len(raw) > 256<<10 {
			return nil, ErrUnavailable
		}
		// Both owners may wrap the canonical payload in their existing data envelope.
		var envelope struct {
			Data  json.RawMessage `json:"data"`
			Error json.RawMessage `json:"error"`
		}
		if json.Unmarshal(raw, &envelope) != nil {
			return nil, ErrUnavailable
		}
		if len(envelope.Error) > 0 && !bytes.Equal(bytes.TrimSpace(envelope.Error), []byte("null")) {
			return nil, ErrUnavailable
		}
		if len(envelope.Data) > 0 {
			raw = envelope.Data
		}
		var result struct {
			Items []struct {
				ID         string      `json:"id"`
				Visible    *bool       `json:"visible"`
				Projection *Projection `json:"projection,omitempty"`
			} `json:"items"`
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&result) != nil || decoder.Decode(&struct{}{}) != io.EOF || len(result.Items) != len(selected) {
			return nil, ErrUnavailable
		}
		seen := map[string]bool{}
		for _, item := range result.Items {
			r, exists := known[item.ID]
			if !exists || seen[item.ID] || item.Visible == nil {
				return nil, ErrUnavailable
			}
			seen[item.ID] = true
			if !*item.Visible {
				if item.Projection != nil {
					return nil, ErrUnavailable
				}
				continue
			}
			if item.Projection == nil || !validProjection(*item.Projection, r) {
				return nil, ErrUnavailable
			}
			out[item.ID] = *item.Projection
		}
	}
	return out, nil
}
