package obs_test

import (
	"context"
	"ora/internal/obs"
	"testing"
)

func TestInitTelemetry(t *testing.T) {
	ctx := context.Background()

	// initialize telemetry hub
	shutdown, err := obs.InitTelemetry(ctx, true)
	if err != nil {
		t.Fatalf("Failed to initialize telemetry: %+v", err)
	}

	//
	if err := shutdown(ctx); err != nil {
		t.Errorf("Shutdown failed: %+v", err)
	}

	t.Log("Telemetry Hub initialized and shut down successfully.")
}
