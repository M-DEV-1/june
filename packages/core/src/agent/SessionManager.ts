import { randomUUID } from 'node:crypto';
import {
    LlmAgent,
    InMemorySessionService,
    Runner,
    StreamingMode,
    LiveRequestQueue,
    type RunConfig,
    InvocationContext
} from '@google/adk';
import { AppMessage } from '../types/index.js';
import { micCapture } from '../audio/MicCapture.js';
import { speakerOutput } from '../audio/SpeakerOutput.js';
import { eventBus } from './EventBus.js';
import { contextManager } from '../memory/ContextManager.js';
import { toolResultCompressor } from '../memory/ToolResultCompressor.js';

interface AdkEventPart {
    text?: string;
    thought?: string;
    inlineData?: {
        data: string;
        mimeType: string;
    };
    functionCall?: {
        name: string;
        args: Record<string, unknown>;
    };
    functionResponse?: {
        name: string;
        content: unknown;
    };
}

interface AdkEvent {
    content?: {
        parts?: AdkEventPart[];
    };
    serverContent?: {
        modelTurn?: {
            parts?: AdkEventPart[];
        };
    };
    inputTranscription?: {
        text?: string;
    };
    toolCall?: {
        name: string;
        args: Record<string, unknown>;
    };
    toolResponse?: {
        name: string;
        content: unknown;
    };
    turnComplete?: boolean;
    interrupted?: boolean;
}

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
        this.currentUserId = randomUUID();
        this.currentSessionId = randomUUID();

        process.env['GEMINI_API_KEY'] = apiKey;

        await this.sessionService.createSession({ 
            appName: 'ora', 
            userId: this.currentUserId, 
            sessionId: this.currentSessionId 
        });

        this.runner = new Runner({
            appName: 'ora',
            agent: this.agent,
            sessionService: this.sessionService,
        });

        this.liveRequestQueue = new LiveRequestQueue();

        const modalities = ["AUDIO", "TEXT"] as any[];

        const runConfig: RunConfig = {
            streamingMode: StreamingMode.BIDI,
            responseModalities: modalities,
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

        const fullSession = await this.sessionService.getSession({
            appName: 'ora',
            userId: this.currentUserId,
            sessionId: this.currentSessionId
        });

        if (!fullSession) throw new Error("Failed to create session");

        // Manually create InvocationContext to include liveRequestQueue
        const invocationContext = new InvocationContext({
            invocationId: randomUUID(),
            agent: this.agent,
            session: fullSession,
            userContent: { 
                parts: [{ text: `${this.agent.instruction}\n\n${contextManager.getSystemPromptPrefix()}System Booted. Neural Interface Online.` }] 
            } as any,
            runConfig,
            liveRequestQueue: this.liveRequestQueue,
            pluginManager: this.runner?.pluginManager
        });

        eventBus.emitEvent({ type: 'status', status: 'LISTENING' });

        try {
            // Call agent.runAsync directly with the context that has the queue
            const stream = this.agent.runAsync(invocationContext);
            for await (const event of stream) {
                await this.handleEvent(event as AdkEvent);
            }
        } catch (error: unknown) {
            const errorMessage = error instanceof Error ? error.message : String(error);
            eventBus.emitEvent({ type: 'error', message: errorMessage, recoverable: true });
            eventBus.emitEvent({ type: 'status', status: 'ERROR' });
        } finally {
            this.stop();
        }
    }

    private async handleEvent(event: AdkEvent) {
        // Handle Transcript & Thought
        if (event.content?.parts) {
            const textParts = event.content.parts.filter(p => p.text).map(p => p.text as string);
            const text = textParts.join('');
            
            const thoughtParts = event.content.parts.filter(p => p.thought).map(p => p.thought as string);
            const thoughts = thoughtParts.join('');

            if (thoughts) {
                eventBus.emitEvent({ 
                    type: 'tool:start', 
                    agent: this.agent.name, 
                    args: {}, 
                    tool: '', 
                    thought: thoughts 
                });
            }

            if (text) {
                eventBus.emitEvent({ type: 'transcript', role: 'agent', text, partial: false, timestamp: Date.now() });
                eventBus.emitEvent({ type: 'status', status: 'SPEAKING' });
                await contextManager.ingestTurn({ role: 'agent', text });
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
            eventBus.emitEvent({ type: 'transcript', role: 'user', text: event.inputTranscription.text, partial: false, timestamp: Date.now() });
            await contextManager.ingestTurn({ role: 'user', text: event.inputTranscription.text });
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
            const result = event.toolResponse.content;
            await toolResultCompressor.compress(event.toolResponse.name, result, "implicit research goal");

            eventBus.emitEvent({ 
                type: 'tool:end', 
                agent: this.agent.name, 
                tool: event.toolResponse.name, 
                durationMs: 0, 
                result: result 
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
    }

    toggleMute() {
        this.isMuted = !this.isMuted;
        return this.isMuted;
    }
}
