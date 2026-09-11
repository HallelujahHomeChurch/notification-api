package eligibilityclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/HallelujahHomeChurch/notification-api/internal/contracts"
)

type Client struct {
	baseURL string
	http    *http.Client
}

func New(baseURL string) *Client {
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), http: &http.Client{Timeout: 8 * time.Second}}
}

func (c *Client) Check(ctx context.Context, ref contracts.EligibilityRef) (bool, error) {
	body, _ := json.Marshal(ref)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/priv/campaign-deliveries/eligibility", bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	request.Header.Set("Content-Type", "application/json")
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
	}
	if json.NewDecoder(response.Body).Decode(&result) != nil {
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
