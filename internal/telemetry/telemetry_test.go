package telemetry

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	collect "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func recordingProvider(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	old := otel.GetTracerProvider()
	exp := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()); otel.SetTracerProvider(old) })
	return exp
}
func TestHTTPParentChildAndPrivacy(t *testing.T) {
	exp := recordingProvider(t)
	const parent = "00-12345678901234567890123456789012-1234567890123456-01"
	req := httptest.NewRequest("GET", "http://example.test/private/recipient?token=secret", nil)
	req.Header.Set("traceparent", parent)
	req.Header.Set("Cookie", "secret")
	req.Header.Set("baggage", "secret")
	req.Header.Set("tracestate", "secret")
	var incoming trace.SpanContext
	handler := Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		incoming = trace.SpanContextFromContext(r.Context())
		if r.Header.Get("Cookie") != "secret" {
			t.Fatal("handler headers changed")
		}
		out, _ := http.NewRequestWithContext(r.Context(), "POST", "http://127.0.0.1:3500/v1.0/invoke/account-api/method/private?secret=token", nil)
		transport := Transport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
			sc := trace.SpanContextFromContext(ExtractParent(context.Background(), r.Header.Get("traceparent")))
			if !sc.IsValid() || sc.TraceID() != incoming.TraceID() || sc.SpanID() == incoming.SpanID() {
				t.Fatal("missing client child")
			}
			return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("secret"))}, nil
		}))
		response, err := transport.RoundTrip(out)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.ReadAll(response.Body)
		_ = response.Body.Close()
		w.WriteHeader(404)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), req)
	spans := exp.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("spans=%d", len(spans))
	}
	if spans[0].Parent.SpanID() != incoming.SpanID() || spans[1].Parent.SpanID().String() != "1234567890123456" {
		t.Fatal("wrong parent chain")
	}
	for _, s := range spans {
		if s.Name != "http.client" && s.Name != "http.server" {
			t.Fatal("unbounded span name")
		}
		if s.Status.Description != "" || len(s.Events) != 0 || s.SpanContext.TraceState().Len() != 0 {
			t.Fatal("private span metadata")
		}
		for _, attr := range s.Attributes {
			if attr.Key != "http.request.method" && attr.Key != "http.response.status_code" {
				t.Fatalf("unexpected attribute %s", attr.Key)
			}
		}
	}
}
func TestHealthAndExternalRequestsAreNotInstrumented(t *testing.T) {
	exp := recordingProvider(t)
	Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := w.(http.Flusher); !ok {
			t.Fatal("Flusher lost")
		}
		w.WriteHeader(200)
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "http://local/health", nil))
	request, _ := http.NewRequest("GET", "https://external.test/signed?secret=token", nil)
	_, err := Transport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r != request {
			t.Fatal("external request changed")
		}
		return &http.Response{StatusCode: 200}, nil
	})).RoundTrip(request)
	if err != nil || len(exp.GetSpans()) != 0 {
		t.Fatal("unexpected instrumentation")
	}
	Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := w.(http.Flusher); !ok {
			t.Fatal("Flusher lost")
		}
		w.WriteHeader(200)
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "http://local/private", nil))
}
func TestStrictParentAndSamplingBounds(t *testing.T) {
	for _, value := range []string{"bad", "00-00000000000000000000000000000000-1234567890123456-01", "00-12345678901234567890123456789012-0000000000000000-01", "00-12345678901234567890123456789012-1234567890123456-ff"} {
		if trace.SpanContextFromContext(ExtractParent(context.Background(), value)).IsValid() {
			t.Fatalf("accepted %q", value)
		}
	}
	for _, value := range []string{"", "bad", "NaN", "Inf", "-0.1", "1"} {
		t.Setenv("OTEL_TRACES_SAMPLER_ARG", value)
		if sampleRatio() != 0.01 {
			t.Fatal("unsafe ratio")
		}
	}
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "0.05")
	if sampleRatio() != 0.05 {
		t.Fatal("valid ratio rejected")
	}
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	old := otel.GetTracerProvider()
	Init(context.Background(), service)()
	if old != otel.GetTracerProvider() {
		t.Fatal("disabled export replaced provider")
	}
}

type collector struct {
	collect.UnimplementedTraceServiceServer
	received chan *collect.ExportTraceServiceRequest
}

func (c *collector) Export(ctx context.Context, req *collect.ExportTraceServiceRequest) (*collect.ExportTraceServiceResponse, error) {
	c.received <- req
	return &collect.ExportTraceServiceResponse{}, nil
}
func TestOTLPExport(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	c := &collector{received: make(chan *collect.ExportTraceServiceRequest, 1)}
	collect.RegisterTraceServiceServer(server, c)
	go server.Serve(listener)
	defer server.Stop()
	old := otel.GetTracerProvider()
	defer otel.SetTracerProvider(old)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://"+listener.Addr().String())
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "0.1")
	t.Setenv("RELEASE", "test-release")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "member.secret=sentinel,service.name=private-name")
	shutdown := Init(context.Background(), service)
	ctx := ExtractParent(context.Background(), "00-00000000000000000000000000000001-1234567890123456-01")
	_, span := otel.Tracer(service).Start(ctx, "test.export")
	span.End()
	shutdown()
	select {
	case req := <-c.received:
		if len(req.ResourceSpans) != 1 || len(req.ResourceSpans[0].Resource.Attributes) != 3 {
			t.Fatal("unexpected resource metadata")
		}
		for _, attr := range req.ResourceSpans[0].Resource.Attributes {
			if attr.Key != "service.name" && attr.Key != "service.version" && attr.Key != "deployment.environment.name" {
				t.Fatal("environment resource escaped allowlist")
			}
			if strings.Contains(attr.Value.String(), "sentinel") || strings.Contains(attr.Value.String(), "private-name") {
				t.Fatal("private environment metadata exported")
			}
		}
	case <-time.After(time.Second):
		t.Fatal("no real OTLP export")
	}
}

func TestRemoteSampledFlagCannotRaiseLocalBudget(t *testing.T) {
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "0.01")
	selected, _ := trace.TraceIDFromHex("00000000000000000000000000000001")
	rejected, _ := trace.TraceIDFromHex("ffffffffffffffffffffffffffffffff")
	id, _ := trace.SpanIDFromHex("1234567890123456")
	for _, tc := range []struct {
		name            string
		remote, sampled bool
		id              trace.TraceID
		want            sdktrace.SamplingDecision
	}{
		{"remote sampled high", true, true, rejected, sdktrace.Drop},
		{"remote sampled low", true, true, selected, sdktrace.RecordAndSample},
		{"remote unsampled low", true, false, selected, sdktrace.Drop},
		{"local sampled high", false, true, rejected, sdktrace.RecordAndSample},
	} {
		t.Run(tc.name, func(t *testing.T) {
			flags := trace.TraceFlags(0)
			if tc.sampled {
				flags = trace.FlagsSampled
			}
			parent := trace.NewSpanContext(trace.SpanContextConfig{TraceID: tc.id, SpanID: id, TraceFlags: flags, Remote: tc.remote})
			for repeat := 0; repeat < 100; repeat++ {
				got := sampler().ShouldSample(sdktrace.SamplingParameters{ParentContext: trace.ContextWithSpanContext(context.Background(), parent), TraceID: tc.id, Name: "test"})
				if got.Decision != tc.want {
					t.Fatalf("repeat=%d decision=%v want=%v", repeat, got.Decision, tc.want)
				}
			}
		})
	}
}
