package cmd

import (
	"context"
	_ "embed"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"ora/internal/config"
	"ora/internal/db"
	"ora/internal/memory"
	"ora/internal/tracker"

	"github.com/getlantern/systray"
)

//go:embed tray_icon.ico
var trayIcon []byte

const DaemonPort = "6942"

func runDaemon(ctx context.Context, shutdownObs func(context.Context) error) error {
	slog.Info("Starting Ora Daemon...")

	// port binding instance lock to prevent double spawning
	listener, err := net.Listen("tcp", "127.0.0.1:"+DaemonPort)
	if err != nil {
		slog.Warn("failed to bind daemon port", "port", DaemonPort, "error", err)
		return nil
	}

	onReady := func() {
		systray.SetIcon(trayIcon)
		systray.SetTitle("Ora")
		systray.SetTooltip("Ora Context Runtime is active")
		mQuit := systray.AddMenuItem("Quit Ora", "Stop the background tracker and database")

		store, err := db.New("ora-db/db")
		if err != nil {
			slog.Error("failed to init db", "error", err)
			listener.Close()
			return
		}

		eye, err := tracker.New()
		if err != nil {
			slog.Error("failed to init tracker", "error", err)
			listener.Close()
			return
		}

		appConfig := config.LoadConfig()
		apiKey := os.Getenv("GEMINI_API_KEY")

		summarizer, err := memory.NewGeminiSummarizer(apiKey)
		if err != nil {
			slog.Warn("failed to init summarizer, semantic memory disabled", "error", err)
		}
		compiler := memory.NewCompiler(summarizer, store)

		eventChan := make(chan tracker.Activity, 100)
		daemon := tracker.NewDaemon(eye, 2*time.Second, appConfig.Tracker.DwellTime*time.Millisecond, appConfig.Tracker.Blocklist, eventChan)

		// tracker loop entry point
		go daemon.Start(ctx)

		// drop raw activity rows older than 72 h every 6 hours
		go func() {
			t := time.NewTicker(6 * time.Hour)
			defer t.Stop()
			for {
				select {
				case <-t.C:
					n, err := store.CullRawActivities(ctx, 72*time.Hour)
					if err != nil {
						slog.Error("activity cull failed", "error", err)
					} else {
						slog.Info("activity cull complete", "deleted_rows", n)
					}
				case <-ctx.Done():
					return
				}
			}
		}()

		go func() {
			for ev := range eventChan {
				store.LogActivity(ctx, ev.App, ev.Title)
				if compiler != nil {
					compiler.Ingest(ctx, ev)
				}
			}
		}()

		// local http for inter-process communication between tui and daemon
		// ipc is a bridge to securely talk and share data between independent apps
		mux := http.NewServeMux()

		// heartbeat
		mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("pong"))
		})

		// data sharing endpooint, get latest tracking data
		mux.HandleFunc("/buffer", func(w http.ResponseWriter, r *http.Request) {
			if compiler == nil {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			buf := compiler.GetCurrentBuffer()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(buf)
		})

		server := &http.Server{
			Handler: mux,
		}

		go func() {
			if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
				slog.Error("daemon ipc server failed", "error", err)
			}
		}()

		slog.Info("Daemon running in background.")

		// block and wait for quit signal
		go func() {
			select {
			case <-mQuit.ClickedCh:
				slog.Info("Quit requested via System Tray")
				systray.Quit()
			case <-ctx.Done():
				slog.Info("Context cancelled, shutting down systray")
				systray.Quit()
			}

			// cleanup process
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			server.Shutdown(shutdownCtx)
			store.Close()
		}()
	}

	onExit := func() {
		slog.Info("Ora Daemon has completely shut down.")
	}

	systray.Run(onReady, onExit)
	return nil
}
