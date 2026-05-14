package obs

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	"go.opentelemetry.io/otel/trace"
)

var (
	providers = make(map[string]*sdktrace.TracerProvider)
	mu        sync.RWMutex
	exporter  sdktrace.SpanExporter
)

// global slog logger, otel traceprovider init
// returns shutdown, must defer in main.go
func InitTelemetry(ctx context.Context, isTest bool) (func(context.Context) error, error) {
	// we create a log file to move all otel logs
	logDir := "ora-db"
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create log directory: %w", err)
	}

	logFile, err := os.OpenFile(logDir+"/ora.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to open log file: %w", err)
	}

	// setup slog
	// so instead of one big setup, i decided to make it seperated, also created a custom slog Handler. extracts trace, span from context for every log and injects as json
	options := &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}
	jsonHandler := slog.NewJSONHandler(logFile, options)
	logger := slog.New(&TraceHandler{handler: jsonHandler})
	slog.SetDefault(logger)

	if isTest {
		slog.Debug("Running in test mode, bypassing OTLP exporter setup.")
		return func(ctx context.Context) error { return nil }, nil
	}

	// setup otlp trace exporter (for any collector)
	// otlptracegrpc localhost:4317
	// data moves to 4317 and then to any collector (whatever is setup)
	var errExport error
	exporter, errExport = otlptracegrpc.New(ctx, otlptracegrpc.WithInsecure())
	if errExport != nil {
		return nil, fmt.Errorf("failed to create OTLP trace exporter: %w", errExport)
	}

	// set global propagator
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))

	slog.Info("Telemetry baseline initialized", "exporter", "otlp-grpc", "endpoint", "localhost:4317")

	// shutdown
	shutdown := func(shutdownCtx context.Context) error {
		mu.Lock()
		defer mu.Unlock()

		slog.Info("Shutting down segmented telemetry providers...")
		for name, tp := range providers {
			if err := tp.Shutdown(shutdownCtx); err != nil {
				slog.Error("failed to shutdown provider", "service", name, "error", err)
			}
		}
		return nil
	}

	return shutdown, nil
}

// GetTracer returns a tracer for a specific logical service.
// should create segmented view in jaeger or whatever
func GetTracer(ctx context.Context, serviceName string) trace.Tracer {
	mu.Lock()
	defer mu.Unlock()

	if tp, ok := providers[serviceName]; ok {
		return tp.Tracer(serviceName)
	}

	// new provider for this logical service
	res, _ := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceNameKey.String(serviceName),
			semconv.ServiceVersionKey.String("1.0.0"),
		),
	)
	// create global tracer provider
	// data batcher, sort of. manages exporter and resource combined
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		// 100% visibility for a local agent is okay
		// TODO: use cloud/local selector if any cloud provider is integrated
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)

	providers[serviceName] = tp

	// don't call otel.SetTracerProvider(tp) because we want to maintain multiple providers
	// for multiple logical services within the same binary.
	// abracadabra magic for tracers
	return tp.Tracer(serviceName)
}

// apparently this is a one time setup file?
// counter for how many times i changed this file: 2

// handler struct to wrap another handler which is the basis of this tracer middleware tbh (json)
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
