package obs

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"ora/internal/config"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	"go.opentelemetry.io/otel/trace"
)

var (
	tp *sdktrace.TracerProvider
)

// global slog logger, otel traceprovider init
// returns shutdown, must defer in main.go
func InitTelemetry(ctx context.Context, isTest bool) (func(context.Context) error, error) {
	// The log goes in config.DataDir(), not a working-directory-relative "ora-db" — the daemon (launched by the autostart entry, cwd = the binary's directory) and a terminal-launched client would otherwise write to two different log files.
	logDir := config.DataDir()
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create log directory: %w", err)
	}

	logFile, err := os.OpenFile(filepath.Join(logDir, "ora.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to open log file: %w", err)
	}

	// custom slog Handler pulls trace/span out of ctx and injects them into every json log line
	options := &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}
	jsonHandler := slog.NewJSONHandler(logFile, options)
	logger := slog.New(&TraceHandler{handler: jsonHandler})
	slog.SetDefault(logger)

	noop := func(ctx context.Context) error { return nil }

	if isTest {
		slog.Debug("Running in test mode, bypassing OTLP exporter setup.")
		return noop, nil
	}

	// otlp trace exporter, defaults to localhost:4317 for any collector (jaeger etc).
	// only enabled when OTEL_EXPORTER_OTLP_ENDPOINT is set -- most won't have a collector running, and blocking on the gRPC dial added 3-5s startup cost.
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		slog.Info("tracing disabled (set OTEL_EXPORTER_OTLP_ENDPOINT=localhost:4317 to enable)")
		return noop, nil
	}

	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithInsecure(),
		otlptracegrpc.WithEndpoint(endpoint),
	)
	if err != nil {
		slog.Warn("failed to create OTLP exporter, tracing disabled", "error", err)
		return noop, nil
	}

	res, _ := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceNameKey.String("ora"),
			semconv.ServiceVersionKey.String("0.1.1"),
		),
	)

	tp = sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)

	otel.SetTracerProvider(tp)
	// set global propagator
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))

	slog.Info("Telemetry baseline initialized", "exporter", "otlp-grpc", "endpoint", "localhost:4317")

	// shutdown
	shutdown := func(shutdownCtx context.Context) error {
		slog.Info("Shutting down telemetry provider...")
		if tp != nil {
			return tp.Shutdown(shutdownCtx)
		}
		return nil
	}

	return shutdown, nil
}

// GetTracer returns a tracer for a specific logical service.
// should create segmented view in jaeger or whatever
func GetTracer(ctx context.Context, name string) trace.Tracer {
	return otel.Tracer(name)
}

// apparently this is a one time setup file?
// counter for how many times i changed this file: 3

// TraceHandler wraps another slog.Handler and injects trace_id/span_id into every log record.
type TraceHandler struct {
	handler slog.Handler
}

func (h *TraceHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.handler.Enabled(ctx, level)
}

func (h *TraceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &TraceHandler{handler: h.handler.WithAttrs(attrs)}
}

func (h *TraceHandler) WithGroup(name string) slog.Handler {
	return &TraceHandler{handler: h.handler.WithGroup(name)}
}

func (h *TraceHandler) Handle(ctx context.Context, r slog.Record) error {
	span := trace.SpanFromContext(ctx)
	if span.SpanContext().IsValid() {
		r.AddAttrs(
			slog.String("trace_id", span.SpanContext().TraceID().String()),
			slog.String("span_id", span.SpanContext().SpanID().String()),
		)
	}
	return h.handler.Handle(ctx, r)
}
