package inbox

import "errors"

type CallerConfig struct{ OperationsAppID, EngagementAppID, GatewayAppID string }

func (c CallerConfig) defaults() CallerConfig {
	if c.OperationsAppID == "" {
		c.OperationsAppID = "operations-api"
	}
	if c.EngagementAppID == "" {
		c.EngagementAppID = "engagement-api"
	}
	if c.GatewayAppID == "" {
		c.GatewayAppID = "api-gateway"
	}
	return c
}
func (c CallerConfig) Validate() error {
	c = c.defaults()
	if (c.OperationsAppID != "operations-api" && c.OperationsAppID != "operations-api-test") || (c.EngagementAppID != "engagement-api" && c.EngagementAppID != "engagement-api-test") || (c.GatewayAppID != "api-gateway" && c.GatewayAppID != "api-gateway-test") || (c.OperationsAppID == "operations-api-test") != (c.EngagementAppID == "engagement-api-test") {
		return errors.New("inbox requires exact same-environment producer IDs and an exact gateway ID")
	}
	return nil
}
