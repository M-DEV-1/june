import { randomUUID } from 'node:crypto';
import {
    LlmAgent,
    InMemorySessionService,
    Runner,
    StreamingMode,
    LiveRequestQueue,
    type RunConfig
} from '@google/adk';
import { micCapture } from '../audio/MicCapture.js';
import { speakerOutput } from '../audio/SpeakerOutput.js';
import { eventBus } from './EventBus.js';

export class SessionManager {
    private runner: Runner | null = null;
    private liveRequestQueue: LiveRequestQueue | null = null;
    private sessionService: InMemorySessionService;
    private isMuted: boolean = false;
    private currentSessionId: string | null = null;
    private currentUserId: string | null = null;

    constructor(private agent: LlmAgent) {
        this.sessionService = new InMemorySessionService();
    }

    async start(apiKey: string) {
        process.env['GEMINI_API_KEY'] = apiKey;
        
        this.currentUserId = randomUUID();
        this.currentSessionId = randomUUID();

        await this.sessionService.createSession({ 
            appName: 'voxpilot-adk', 
            userId: this.currentUserId, 
            sessionId: this.currentSessionId 
        });

        this.runner = new Runner({
            appName: 'voxpilot-adk',
            agent: this.agent,
            sessionService: this.sessionService,
        });

        this.liveRequestQueue = new LiveRequestQueue();

        const runConfig: RunConfig = {
            streamingMode: StreamingMode.BIDI,
            responseModalities: ["AUDIO", "TEXT"] as any,
            maxLlmCalls: 100
        };

        // Start Mic Capture
        const micStream = micCapture.start();
        micStream.on('data', (data: Buffer) => {
            if (this.isMuted) return;
            this.liveRequestQueue?.sendRealtime({
                mimeType: "audio/pcm; rate=16000",
                data: data.toString('base64')
            });
        });

        const stream = this.runner.runAsync({
            userId: this.currentUserId,
            sessionId: this.currentSessionId,
            runConfig,
            liveRequestQueue: this.liveRequestQueue as any,
            newMessage: { parts: [{ text: "System Booted. Neural Interface Online." }] }
        });

        eventBus.emitEvent({ type: 'status', status: 'LISTENING' });

        try {
            for await (const event of stream as any) {
                this.handleEvent(event);
            }
        } catch (error: any) {
            eventBus.emitEvent({ type: 'error', message: error.message, recoverable: true });
            eventBus.emitEvent({ type: 'status', status: 'ERROR' });
        } finally {
            this.stop();
        }
    }

    private handleEvent(event: any) {
        // Handle Transcript
        if (event.content?.parts) {
            const text = event.content.parts.map((p: any) => p.text).join('');
            if (text) {
                eventBus.emitEvent({ type: 'transcript', role: 'agent', text, partial: false });
                eventBus.emitEvent({ type: 'status', status: 'SPEAKING' });
            }
        }

        // Handle Audio
        if (event.serverContent?.modelTurn?.parts) {
            for (const part of event.serverContent.modelTurn.parts) {
                if (part.inlineData) {
                    const buffer = Buffer.from(part.inlineData.data, 'base64');
                    speakerOutput.addChunk(buffer);
                }
            }
        }

        // Handle User Transcript (if available from model)
        if (event.inputTranscription?.text) {
            eventBus.emitEvent({ type: 'transcript', role: 'user', text: event.inputTranscription.text, partial: false });
        }

        // Handle Tool Calls
        if (event.toolCall) {
            eventBus.emitEvent({ 
                type: 'tool:start', 
                agent: this.agent.name, 
                tool: event.toolCall.name, 
                args: event.toolCall.args 
            });
            eventBus.emitEvent({ type: 'status', status: 'PROCESSING' });
        }

        if (event.toolResponse) {
            eventBus.emitEvent({ 
                type: 'tool:end', 
                agent: this.agent.name, 
                tool: event.toolResponse.name, 
                durationMs: 0, // ADK doesn't provide this directly here
                result: event.toolResponse.content 
            });
            eventBus.emitEvent({ type: 'status', status: 'LISTENING' });
        }

        if (event.turnComplete || event.interrupted) {
            if (event.interrupted) {
                speakerOutput.interrupt();
            }
            eventBus.emitEvent({ type: 'status', status: 'LISTENING' });
        }
    }

    stop() {
        micCapture.stop();
        speakerOutput.stop();
        if (this.liveRequestQueue) {
            this.liveRequestQueue.close();
            this.liveRequestQueue = null;
        }
        eventBus.emitEvent({ type: 'session:end' });
    }

    reset() {
        this.stop();
        // UI should trigger restart if needed
    }

    toggleMute() {
        this.isMuted = !this.isMuted;
        return this.isMuted;
    }
}
