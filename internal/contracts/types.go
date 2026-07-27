package contracts

type MessageStatus string

const (
	MessageStatusQueued       MessageStatus = "queued"
	MessageStatusSending      MessageStatus = "sending"
	MessageStatusSent         MessageStatus = "sent"
	MessageStatusFailed       MessageStatus = "failed"
	MessageStatusDeadLettered MessageStatus = "dead_lettered"
)

type DeliveryStatus string

const (
	DeliveryStatusQueued       DeliveryStatus = "queued"
	DeliveryStatusSending      DeliveryStatus = "sending"
	DeliveryStatusSent         DeliveryStatus = "sent"
	DeliveryStatusFailed       DeliveryStatus = "failed"
	DeliveryStatusDeadLettered DeliveryStatus = "dead_lettered"
)

type SendRequest struct {
	TemplateID string            `json:"templateId"`
	Channel    string            `json:"channel"`
	Target     Target            `json:"target"`
	Locale     string            `json:"locale"`
	Payload    map[string]string `json:"payload"`
	Resource   Resource          `json:"resource"`
}

type Target struct {
	Type    string `json:"type"`
	Address string `json:"address"`
}

type Resource struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

type SendResponse struct {
	Data  SendResponseData `json:"data"`
	Meta  ResponseMeta     `json:"meta"`
	Error *ResponseError   `json:"error"`
}

type SendResponseData struct {
	MessageID       string        `json:"messageId"`
	Status          MessageStatus `json:"status"`
	TemplateVersion int           `json:"templateVersion"`
	Replayed        bool          `json:"replayed"`
}

type ResponseMeta struct {
	RequestID string `json:"requestId"`
}

type ResponseError struct {
	Code string `json:"code"`
}
