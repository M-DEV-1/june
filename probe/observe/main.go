// Calls tracker.Observe on the window in front, skipping Ora's own windows, and prints what the model would see, with the timing and size. Waits up to 20 s for a window to take focus, since the watcher only learns focus from activation events after it starts.
package main

import (
	"context"
	"fmt"
	"time"

	"ora/internal/act"
	"ora/internal/tracker"
)

func main() {
	deadline := time.Now().Add(20 * time.Second)
	for {
		start := time.Now()
		app, title, nodes, err := tracker.Observe(context.Background())
		if (err != nil || tracker.IsOraWindow(app, title)) && time.Now().Before(deadline) {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		if err != nil {
			fmt.Println("error:", err)
			return
		}
		items := act.Filter(nodes)
		text := act.Format(items)
		fmt.Printf("%s · %s\nraw nodes %d, kept %d, %d bytes, walk %s\n\n%s\n", app, title, len(nodes), len(items), len(text), time.Since(start).Round(time.Millisecond), text)
		return
	}
}
