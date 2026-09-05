// Probe: opens one Live API session on the model named in argv[1] (default gemini-3.1-flash-live-preview), says "hi", and prints the first server message or the close error. Run: cd ~/Desktop/Code/projects/ora && set -a && source .env && set +a && go run ~/ora-probe/main.go
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"strings"

	"path/filepath"

	"ora/internal/agent"
	"ora/internal/db"

	"google.golang.org/genai"
)

func main() {
	model := "gemini-3.1-flash-live-preview"
	if len(os.Args) > 1 {
		model = os.Args[1]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cc := &genai.ClientConfig{APIKey: os.Getenv("GEMINI_API_KEY"), Backend: genai.BackendGeminiAPI}
	if os.Getenv("PROBE_ALPHA") != "" {
		cc.HTTPOptions = genai.HTTPOptions{APIVersion: "v1alpha"}
	}
	client, err := genai.NewClient(ctx, cc)
	if err != nil {
		fmt.Println("client:", err)
		return
	}
	cfg := &genai.LiveConnectConfig{ResponseModalities: []genai.Modality{genai.ModalityAudio}, OutputAudioTranscription: &genai.AudioTranscriptionConfig{}}
	if n := os.Getenv("PROBE_BIG_KB"); n != "" {
		var kb int
		fmt.Sscan(n, &kb)
		cfg.SystemInstruction = &genai.Content{Role: "system", Parts: []*genai.Part{{Text: "You are Ora. Background notes follow.\n" + strings.Repeat("The user worked on the climate risk statement builder and reviewed pull requests with colleagues. ", kb*1024/96)}}}
	}
	if v := os.Getenv("PROBE_VOICE"); v != "" {
		cfg.SpeechConfig = &genai.SpeechConfig{VoiceConfig: &genai.VoiceConfig{PrebuiltVoiceConfig: &genai.PrebuiltVoiceConfig{VoiceName: v}}}
	}
	if os.Getenv("PROBE_AFFECT") != "" {
		cfg.EnableAffectiveDialog = genai.Ptr(true)
	}
	if os.Getenv("PROBE_PROACTIVE") != "" {
		cfg.Proactivity = &genai.ProactivityConfig{ProactiveAudio: genai.Ptr(true)}
	}
	if v := os.Getenv("PROBE_VAD_MS"); v != "" {
		var ms int32
		fmt.Sscan(v, &ms)
		cfg.RealtimeInputConfig = &genai.RealtimeInputConfig{AutomaticActivityDetection: &genai.AutomaticActivityDetection{SilenceDurationMs: genai.Ptr(ms), EndOfSpeechSensitivity: genai.EndSensitivityHigh, StartOfSpeechSensitivity: genai.StartSensitivityHigh}}
	}
	if os.Getenv("PROBE_THINK") != "" {
		cfg.ThinkingConfig = &genai.ThinkingConfig{IncludeThoughts: true, ThinkingLevel: genai.ThinkingLevelLow}
	}
	if os.Getenv("PROBE_REAL") != "" {
		home, _ := os.UserHomeDir()
		store, err := db.New(filepath.Join(home, ".local/share/ora/db"))
		if err != nil {
			fmt.Println("open store:", err)
			return
		}
		impl, _ := store.GetImplicitContext(ctx)
		pc, _ := store.PersonalContext(ctx)
		store.Close()
		text := agent.SystemInstruction(time.Now(), agent.PersonalContextBlock(pc), strings.Join(impl, "\n"))
		fmt.Println("real system instruction bytes:", len(text))
		cfg.SystemInstruction = &genai.Content{Role: "system", Parts: []*genai.Part{{Text: text}}}
		cfg.Tools = []*genai.Tool{{FunctionDeclarations: agent.ToolDeclarations()}}
	}
	if os.Getenv("PROBE_SEARCH") != "" {
		cfg.Tools = []*genai.Tool{{FunctionDeclarations: agent.ToolDeclarations()}, {GoogleSearch: &genai.GoogleSearch{}}}
	}
	if os.Getenv("PROBE_TOOLS") != "" {
		cfg.Tools = []*genai.Tool{{FunctionDeclarations: agent.ToolDeclarations()}}
	}
	if os.Getenv("PROBE_RESUME") != "" {
		cfg.SessionResumption = &genai.SessionResumptionConfig{}
		cfg.ContextWindowCompression = &genai.ContextWindowCompressionConfig{TriggerTokens: genai.Ptr[int64](64000), SlidingWindow: &genai.SlidingWindow{TargetTokens: genai.Ptr[int64](32000)}}
		cfg.InputAudioTranscription = &genai.AudioTranscriptionConfig{}
		cfg.RealtimeInputConfig = &genai.RealtimeInputConfig{AutomaticActivityDetection: &genai.AutomaticActivityDetection{PrefixPaddingMs: genai.Ptr[int32](300)}}
	}
	s, err := client.Live.Connect(ctx, model, cfg)
	if err != nil {
		fmt.Println("connect:", err)
		return
	}
	defer s.Close()
	if os.Getenv("PROBE_AUDIO") != "" || os.Getenv("PROBE_AUDIO_THEN_IDLE") != "" {
		// Five seconds of silent 16 kHz PCM, the way Ora's mic loop streams from the first moment.
		silence := make([]byte, 16000*2/10)
		for i := 0; i < 50; i++ {
			if err := s.SendRealtimeInput(genai.LiveSendRealtimeInputParameters{Audio: &genai.Blob{Data: silence, MIMEType: "audio/pcm;rate=16000"}}); err != nil {
				fmt.Println("audio send:", err)
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	if os.Getenv("PROBE_IDLE") != "" || os.Getenv("PROBE_AUDIO_THEN_IDLE") != "" {
		// Send nothing at all and report how long the server keeps an idle session open, and with what close reason. This is what Ora looks like with the microphone muted.
		start := time.Now()
		if ka := os.Getenv("PROBE_KEEPALIVE"); ka != "" {
			// A short silent chunk every ka seconds, to learn whether any client message resets the idle clock.
			var secs int
			fmt.Sscan(ka, &secs)
			go func() {
				silence := make([]byte, 16000*2/10)
				for range time.Tick(time.Duration(secs) * time.Second) {
					if err := s.SendRealtimeInput(genai.LiveSendRealtimeInputParameters{Audio: &genai.Blob{Data: silence, MIMEType: "audio/pcm;rate=16000"}}); err != nil {
						fmt.Println("keepalive send:", err)
						return
					}
				}
			}()
		}
		if at := os.Getenv("PROBE_TURN_AT"); at != "" {
			// One text turn after at seconds, then silence: does a completed turn stop the idle clock?
			var secs int
			fmt.Sscan(at, &secs)
			go func() {
				time.Sleep(time.Duration(secs) * time.Second)
				if err := s.SendClientContent(genai.LiveSendClientContentParameters{Turns: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: "hello"}}}}, TurnComplete: genai.Ptr(true)}); err != nil {
					fmt.Println("turn send:", err)
				}
			}()
		}
		deadline := time.After(7 * time.Minute)
		done := make(chan error, 1)
		go func() {
			for {
				if _, err := s.Receive(); err != nil {
					done <- err
					return
				}
			}
		}()
		select {
		case err := <-done:
			fmt.Printf("IDLE: %s closed after %s: %v\n", model, time.Since(start).Round(time.Second), err)
		case <-deadline:
			fmt.Printf("IDLE: %s still open after %s\n", model, time.Since(start).Round(time.Second))
		}
		return
	}
	if wav := os.Getenv("PROBE_WAV"); wav != "" {
		// Speak a recorded question to the model in real time and time what comes back: first audio, spoken text, tool calls. On a tool call, answer it after PROBE_TOOL_DELAY seconds (default 3) with a canned result, to see whether the model gives one word of status before the answer, the way the GPT-Live demo does.
		pcm, rate, err := readWAV(wav)
		if err != nil {
			fmt.Println("wav:", err)
			return
		}
		delay := 3.0
		if d := os.Getenv("PROBE_TOOL_DELAY"); d != "" {
			fmt.Sscan(d, &delay)
		}
		start := time.Now()
		chunk := rate * 2 / 10 // 100 ms of 16-bit mono
		for i := 0; i < len(pcm); i += chunk {
			end := i + chunk
			if end > len(pcm) {
				end = len(pcm)
			}
			if err := s.SendRealtimeInput(genai.LiveSendRealtimeInputParameters{Audio: &genai.Blob{Data: pcm[i:end], MIMEType: fmt.Sprintf("audio/pcm;rate=%d", rate)}}); err != nil {
				fmt.Println("audio send:", err)
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		spokeEnd := time.Now()
		fmt.Printf("%6.2fs  speech sent (%.1fs of audio)\n", spokeEnd.Sub(start).Seconds(), float64(len(pcm))/float64(rate*2))
		// The mic never stops in Ora, so the server's voice detection sees silence after speech. Keep feeding 100 ms silent chunks for the rest of the run so the end of the question is detectable and the session is not idle.
		go func() {
			silence := make([]byte, chunk)
			for {
				if err := s.SendRealtimeInput(genai.LiveSendRealtimeInputParameters{Audio: &genai.Blob{Data: silence, MIMEType: fmt.Sprintf("audio/pcm;rate=%d", rate)}}); err != nil {
					return
				}
				time.Sleep(100 * time.Millisecond)
			}
		}()
		since := func() string { return fmt.Sprintf("%6.2fs", time.Since(spokeEnd).Seconds()) }
		firstAudio, toolCalls := false, 0
		var pendingText string
		deadline := time.After(25 * time.Second)
		msgs := make(chan *genai.LiveServerMessage, 64)
		errc := make(chan error, 1)
		go func() {
			for {
				m, err := s.Receive()
				if err != nil {
					errc <- err
					return
				}
				msgs <- m
			}
		}()
		for {
			select {
			case <-deadline:
				fmt.Println("        stop: 25 s after speech")
				return
			case err := <-errc:
				fmt.Println("receive:", err)
				return
			case msg := <-msgs:
				if sc := msg.ServerContent; sc != nil {
					if sc.ModelTurn != nil {
						for _, p := range sc.ModelTurn.Parts {
							if p.InlineData != nil && !firstAudio {
								firstAudio = true
								fmt.Printf("%s  FIRST AUDIO from the model\n", since())
							}
						}
					}
					if sc.OutputTranscription != nil && sc.OutputTranscription.Text != "" {
						pendingText += sc.OutputTranscription.Text
						if strings.ContainsAny(sc.OutputTranscription.Text, ".?!") || len(pendingText) > 60 {
							fmt.Printf("%s  says: %q\n", since(), strings.TrimSpace(pendingText))
							pendingText = ""
						}
					}
					if sc.Interrupted {
						fmt.Printf("%s  (interrupted)\n", since())
					}
					if sc.TurnComplete || sc.GenerationComplete {
						if pendingText != "" {
							fmt.Printf("%s  says: %q\n", since(), strings.TrimSpace(pendingText))
							pendingText = ""
						}
						fmt.Printf("%s  turn complete\n", since())
						if w2 := os.Getenv("PROBE_WAV2"); w2 != "" && toolCalls == 0 {
							os.Unsetenv("PROBE_WAV2")
							pcm2, rate2, err := readWAV(w2)
							if err != nil {
								fmt.Println("wav2:", err)
								return
							}
							time.Sleep(1500 * time.Millisecond)
							for i := 0; i < len(pcm2); i += chunk {
								end := i + chunk
								if end > len(pcm2) {
									end = len(pcm2)
								}
								_ = s.SendRealtimeInput(genai.LiveSendRealtimeInputParameters{Audio: &genai.Blob{Data: pcm2[i:end], MIMEType: fmt.Sprintf("audio/pcm;rate=%d", rate2)}})
								time.Sleep(100 * time.Millisecond)
							}
							spokeEnd = time.Now()
							firstAudio = false
							fmt.Printf("  ----  second question sent (%.1fs of audio); times below are from its end\n", float64(len(pcm2))/float64(rate2*2))
							deadline = time.After(25 * time.Second)
							continue
						}
						if toolCalls == 0 || firstAudio {
							// give a delegated answer time to be spoken
							if toolCalls > 0 {
								time.Sleep(6 * time.Second)
								for len(msgs) > 0 {
									m := <-msgs
									if m.ServerContent != nil && m.ServerContent.OutputTranscription != nil {
										fmt.Printf("%s  says: %q\n", since(), strings.TrimSpace(m.ServerContent.OutputTranscription.Text))
									}
								}
							}
							return
						}
					}
				}
				if msg.ToolCall != nil {
					for _, fc := range msg.ToolCall.FunctionCalls {
						toolCalls++
						fmt.Printf("%s  TOOL CALL %s %v\n", since(), fc.Name, fc.Args)
						go func(fc *genai.FunctionCall) {
							time.Sleep(time.Duration(delay * float64(time.Second)))
							resp := &genai.FunctionResponse{ID: fc.ID, Name: fc.Name, Response: map[string]any{"output": "Arts Cafe by Strawberry Hill, or Tartine in the Inner Sunset.", "scheduling": "INTERRUPT"}, Scheduling: genai.FunctionResponseSchedulingInterrupt}
							if err := s.SendToolResponse(genai.LiveSendToolResponseParameters{FunctionResponses: []*genai.FunctionResponse{resp}}); err != nil {
								fmt.Println("tool response:", err)
							}
							fmt.Printf("%s  tool result delivered (%.0fs later)\n", since(), delay)
						}(fc)
					}
				}
			}
		}
	}
	if err := s.SendClientContent(genai.LiveSendClientContentParameters{Turns: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: "Say hi in three words."}}}}}); err != nil {
		fmt.Println("send:", err)
		return
	}
	for {
		msg, err := s.Receive()
		if err != nil {
			fmt.Println("receive:", err)
			return
		}
		if msg.ServerContent != nil && msg.ServerContent.OutputTranscription != nil {
			fmt.Print(msg.ServerContent.OutputTranscription.Text)
		}
		if msg.ServerContent != nil && msg.ServerContent.TurnComplete {
			fmt.Println("\nOK:", model, "answered")
			return
		}
	}
}

// readWAV returns the 16-bit mono PCM and sample rate of a WAV file, assuming a plain 44-byte header.
func readWAV(path string) ([]byte, int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	if len(b) < 44 || string(b[0:4]) != "RIFF" {
		return nil, 0, fmt.Errorf("not a RIFF wav")
	}
	rate := int(b[24]) | int(b[25])<<8 | int(b[26])<<16 | int(b[27])<<24
	return b[44:], rate, nil
}
