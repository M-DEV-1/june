package obs

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
)

// global slog logger, otel traceprovider init
// returns shutdown, must defer in main.go
func InitTelemetry(ctx context.Context, isTest bool) (func(context.Context) error, error) {
	// setup slog
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
	slog.SetDefault(logger)

	if isTest {
		slog.Debug("Running in test mode, bypassing OTLP exporter setup.")
		return func(ctx context.Context) error { return nil }, nil
	}

	// setup otlp trace exporter (for any collector)
	// otlptracegrpc localhost:4317
	// data moves to 4317 and then to any collector (whatever is setup)
	exporter, err := otlptracegrpc.New(ctx, otlptracegrpc.WithInsecure())
	if err != nil {
		return nil, fmt.Errorf("failed to create OTLP trace exporter: %w", err)
	}

	// identity of application, i.e., ora v1
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceNameKey.String("ora"),
			semconv.ServiceVersionKey.String("1.0.0"),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create tracing resource: %w", err)
	}

	// create global tracer provider
	// data batcher, sort of. manages exporter and resource combined
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		// 100% visibility for a local agent is okay
		// TODO: use cloud/local selector if any cloud provider is integrated
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)

	// global hook abracadabra magic
	otel.SetTracerProvider(tp)

	slog.Info("Telemetry initialized", "exporter", "otlp-grpc", "endpoint", "localhost:4317")

	// shutdown
	shutdown := func(shutdownCtx context.Context) error {
		slog.Info("Shutting down Telemetry, flushing traces..")
		ctx, cancel := context.WithTimeout(shutdownCtx, 5*time.Second)
		defer cancel()

		if err := tp.Shutdown(ctx); err != nil {
			return fmt.Errorf("failed to shutdown TracerProvider: %w", err)
		}
		return nil
	}

	return shutdown, nil
}

// apparently this is a one time setup file?
// counter for how many times i changed this file: 0
