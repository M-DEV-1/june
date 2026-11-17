package dream

import (
	"context"
	"net"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"
)

// fakeLlamaEnv marks a test-binary run as the stand-in for llama-server that the shadow lifecycle tests spawn.
const fakeLlamaEnv = "JUNE_TEST_FAKE_LLAMA_SERVER"

// TestMain lets the test binary double as llama-server: when fakeLlamaEnv is set it refuses to start if a device is pinned, as llama-server does for a device that is not there, and otherwise serves /health on the --port it was given.
func TestMain(m *testing.M) {
	if os.Getenv(fakeLlamaEnv) == "" {
		os.Exit(m.Run())
	}
	port := ""
	for i, a := range os.Args {
		if a == "-dev" {
			os.Exit(1)
		}
		if a == "--port" && i+1 < len(os.Args) {
			port = os.Args[i+1]
		}
	}
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	http.ListenAndServe("127.0.0.1:"+port, nil)
}

// A pinned device that llama-server refuses is dropped so the shadow still starts, as the embedding server does.
func TestShadowLifecycle_StartsWithoutADeviceThatIsNotThere(t *testing.T) {
	t.Setenv(fakeLlamaEnv, "1")
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	life := NewLlamaServerLifecycle(os.Args[0], "model.gguf", port, "Vulkan1")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := life.Start(ctx); err != nil {
		t.Fatalf("Start with a device llama-server refuses = %v, want it started without the device", err)
	}
	defer life.Stop()

	resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/health")
	if err != nil {
		t.Fatalf("the shadow server is not answering: %v", err)
	}
	resp.Body.Close()
}
