package inbox

import "testing"

func TestCallerConfigSelectsOneEnvironment(t *testing.T) {
	for _, c := range []CallerConfig{{}, {"operations-api-test", "engagement-api-test", "api-gateway"}, {"operations-api-test", "engagement-api-test", "api-gateway-test"}} {
		if e := c.Validate(); e != nil {
			t.Fatal(e)
		}
	}
	for _, c := range []CallerConfig{{"operations-api-test", "engagement-api", "api-gateway"}, {"operations-api-other", "engagement-api", "api-gateway"}, {"operations-api", "engagement-api", "api-gateway,api-gateway-test"}, {"operations-api", "engagement-api", " api-gateway"}} {
		if c.Validate() == nil {
			t.Fatalf("accepted %+v", c)
		}
	}
}
