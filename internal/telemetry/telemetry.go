// Package telemetry records only bounded operational metadata.
package telemetry

import (
	"context"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/felixge/httpsnoop"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const service = "notification-api"

var parentPattern = regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-0[01]$`)

func Init(ctx context.Context, name string) func() {
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" {
		return func() {}
	}
	initCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	exporter, err := otlptracegrpc.New(initCtx)
	if err != nil {
		slog.Warn("trace exporter initialization failed")
		return func() {}
	}
	// Export failures must not disclose endpoint credentials or request errors.
	var exportWarning sync.Once
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(error) { exportWarning.Do(func() { slog.Warn("trace export failed") }) }))
	safeResource := resource.NewSchemaless(attribute.String("service.name", name), attribute.String("service.version", env("RELEASE", env("CONTAINER_APP_REVISION", "unknown"))), attribute.String("deployment.environment.name", env("ENVIRONMENT", "production")))
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(resourceExporter{SpanExporter: exporter, resource: safeResource}, sdktrace.WithMaxQueueSize(512), sdktrace.WithMaxExportBatchSize(128), sdktrace.WithExportTimeout(2*time.Second)),
		sdktrace.WithResource(safeResource),
		sdktrace.WithSampler(sampler()),
	)
	otel.SetTracerProvider(provider)
	return func() {
		shutdownCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		if provider.Shutdown(shutdownCtx) != nil {
			slog.Warn("trace exporter shutdown failed")
		}
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
func sampleRatio() float64 {
	ratio, err := strconv.ParseFloat(os.Getenv("OTEL_TRACES_SAMPLER_ARG"), 64)
	if err != nil || math.IsNaN(ratio) || math.IsInf(ratio, 0) || ratio < 0 || ratio > 0.1 {
		return 0.01
	}
	return ratio
}

// ExtractParent ignores baggage/tracestate and rejects malformed transport context.
func ExtractParent(ctx context.Context, parent string) context.Context {
	if !parentPattern.MatchString(parent) {
		return ctx
	}
	return propagation.TraceContext{}.Extract(ctx, propagation.MapCarrier{"traceparent": parent})
}
func Parent(ctx context.Context) string {
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)
	return carrier["traceparent"]
}

func Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" || r.URL.Path == "/ready" {
			next.ServeHTTP(w, r)
			return
		}
		ctx := r.Context()
		if len(r.Header.Values("traceparent")) == 1 {
			ctx = ExtractParent(ctx, r.Header.Get("traceparent"))
		}
		ctx, span := otel.Tracer(service).Start(ctx, "http.server", trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(attribute.String("http.request.method", method(r.Method))))
		defer span.End()
		metrics := httpsnoop.CaptureMetrics(next, w, r.WithContext(ctx))
		status(span, metrics.Code)
	})
}

func method(value string) string {
	switch value {
	case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "CONNECT", "TRACE":
		return value
	default:
		return "_OTHER"
	}
}
func status(span trace.Span, code int) {
	span.SetAttributes(attribute.Int("http.response.status_code", code))
	if code >= 500 {
		span.SetStatus(codes.Error, "")
	}
}

type transport struct{ base http.RoundTripper }

func Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return transport{base}
}
func (t transport) RoundTrip(r *http.Request) (*http.Response, error) {
	// Only local Dapr invocations receive internal trace context.
	if r.URL.Scheme != "http" || (r.URL.Hostname() != "localhost" && r.URL.Hostname() != "127.0.0.1" && r.URL.Hostname() != "::1") || !strings.HasPrefix(r.URL.Path, "/v1.0/invoke/") {
		return t.base.RoundTrip(r)
	}
	ctx := r.Context()
	if !trace.SpanContextFromContext(ctx).IsValid() && len(r.Header.Values("traceparent")) == 1 {
		ctx = ExtractParent(ctx, r.Header.Get("traceparent"))
	}
	ctx, span := otel.Tracer(service).Start(ctx, "http.client", trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attribute.String("http.request.method", method(r.Method))))
	clone := r.Clone(ctx)
	if parent := Parent(ctx); parent != "" {
		clone.Header.Set("traceparent", parent)
	}
	response, err := t.base.RoundTrip(clone)
	if err != nil {
		span.SetStatus(codes.Error, "")
		span.End()
		return response, err
	}
	status(span, response.StatusCode)
	if response.Body == nil {
		span.End()
	} else {
		response.Body = &body{ReadCloser: response.Body, span: span}
	}
	return response, nil
}

type body struct {
	io.ReadCloser
	span trace.Span
	once sync.Once
}

func (b *body) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		if err != io.EOF {
			b.span.SetStatus(codes.Error, "")
		}
		b.once.Do(func() { b.span.End() })
	}
	return n, err
}
func (b *body) Close() error {
	err := b.ReadCloser.Close()
	if err != nil {
		b.span.SetStatus(codes.Error, "")
	}
	b.once.Do(func() { b.span.End() })
	return err
}

func sampler() sdktrace.Sampler {
	root := sdktrace.TraceIDRatioBased(sampleRatio())
	// Remote sampled flags are correlation, not authority to raise this service's budget.
	return sdktrace.ParentBased(root, sdktrace.WithRemoteParentSampled(root))
}

// Enforce the resource allowlist at export, after SDK environment merging.
type resourceExporter struct {
	sdktrace.SpanExporter
	resource *resource.Resource
}

func (e resourceExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	safe := make([]sdktrace.ReadOnlySpan, len(spans))
	for i, span := range spans {
		safe[i] = resourceSpan{ReadOnlySpan: span, resource: e.resource}
	}
	return e.SpanExporter.ExportSpans(ctx, safe)
}

type resourceSpan struct {
	sdktrace.ReadOnlySpan
	resource *resource.Resource
}

func (s resourceSpan) Resource() *resource.Resource { return s.resource }
