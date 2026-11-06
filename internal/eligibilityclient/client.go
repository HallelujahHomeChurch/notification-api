package eligibilityclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/HallelujahHomeChurch/notification-api/internal/contracts"
)

const engagementOrigin = "https://engagement-api.internal.gentleriver-81abd7bc.eastasia.azurecontainerapps.io"

type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

func New(token string) *Client {
	return &Client{baseURL: engagementOrigin, token: token, http: &http.Client{
		Timeout: 8 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

func (c *Client) Check(ctx context.Context, ref contracts.EligibilityRef) (bool, error) {
	body, _ := json.Marshal(ref)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/priv/notification-worker/campaign-deliveries/eligibility", bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-HHC-Notification-Worker-Eligibility-Token", c.token)
	response, err := c.http.Do(request)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false, errors.New("eligibility unavailable")
	}
	var result struct {
		Data struct {
			Decision string `json:"decision"`
		} `json:"data"`
		Error json.RawMessage `json:"error"`
	}
	const maxResponse = 4096
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponse+1))
	if err != nil || len(data) > maxResponse || json.Unmarshal(data, &result) != nil ||
		(len(result.Error) != 0 && string(result.Error) != "null") {
		return false, errors.New("invalid eligibility response")
	}
	switch result.Data.Decision {
	case "allow":
		return true, nil
	case "suppress":
		return false, nil
	default:
		return false, errors.New("invalid eligibility decision")
	}
}
