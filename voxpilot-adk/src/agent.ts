import {
    LlmAgent,
    GoogleSearchTool,
    InMemorySessionService,
    Runner,
    StreamingMode,
    LiveRequestQueue,
    FunctionTool,
    type RunConfig
} from '@google/adk';
import type { AudioTranscriptionConfig, Blob } from "@google/genai";
import { randomUUID } from 'node:crypto';
import { promises as fs } from 'node:fs';
import { z } from 'zod';
import dotenv from 'dotenv';

dotenv.config();

/* STEPS ---
 - initialize app (startup)
 - session initialization (once per streaming session)
 - active session (concurrent bidirectional communication)
 - session termination
 - loop to step 3 whenever user does /resume
*/

// all ADK bidirectional components must run in an async context - Session Operations, WebSocket operations, Concurrent tasks

export const APP_NAME = "research-voice-agent";
export const createUserId = () => randomUUID();
export const createSessionId = () => randomUUID();

const listDirectory = new FunctionTool({
    name: 'list_directory',
    description: 'Lists files in a given directory path. Use this to find research data or project structures.',
    parameters: z.object({
        path: z.string().default('.').describe('The directory path to list.')
    }),
    execute: async ({ path }) => {
        try {
            const files = await fs.readdir(path);
            return { files };
        } catch (e: any) {
            return { error: `Failed to list directory: ${e.message}` };
        }
    }
});

const readFile = new FunctionTool({
    name: 'read_research_file',
    description: 'Reads the content of a specific file. Necessary for analyzing research data.',
    parameters: z.object({
        path: z.string().describe('Path to the file to read.')
    }),
    execute: async ({ path }) => {
        try {
            const content = await fs.readFile(path, 'utf-8');
            return { content: content.slice(0, 10000) }; // Sufficient context for analysis
        } catch (e: any) {
            return { error: `Failed to read file: ${e.message}` };
        }
    }
});

const saveResearchNotes = new FunctionTool({
    name: 'save_research_notes',
    description: 'Saves synthesized findings or notes to a local file for persistence.',
    parameters: z.object({
        path: z.string().describe('File path where notes should be saved.'),
        content: z.string().describe('The synthesized research notes or summary.')
    }),
    execute: async ({ path, content }) => {
        try {
            await fs.writeFile(path, content, 'utf-8');
            return { success: true, message: `Notes successfully saved to ${path}` };
        } catch (e: any) {
            return { error: `Failed to save notes: ${e.message}` };
        }
    }
});

export const researchAgent = new LlmAgent({
    name: "research_agent",
    model: "gemini-2.0-flash-exp",
    description: "An expert research assistant synthesizing web and local file data via bidirectional voice.",
    instruction: `
        You are a highly capable Research Voice Assistant.
        You monitor local files and the web to synthesize facts and suggest next steps.
        
        Guidelines:
        1. Access local contexts using 'list_directory' and 'read_research_file'.
        2. Use 'google_search' to bridge local data with global knowledge.
        3. Proactively save summaries using 'save_research_notes' when research milestones are reached.
        4. Be concise and insightful in voice responses. Use technical precision.
        5. If you see a file like 'research_log.md', try reading it first to understand current progress.
    `,
    tools: [
        new GoogleSearchTool(),
        listDirectory,
        readFile,
        saveResearchNotes
    ]
});

const sessionService = new InMemorySessionService();
const userId = createUserId();
const sessionId = createSessionId();

export const runner = new Runner({
    appName: APP_NAME,
    agent: researchAgent,
    sessionService: sessionService,
});

const session = await sessionService.getOrCreateSession({
    appName: APP_NAME,
    userId: userId,
    sessionId: sessionId
});

export const runConfig: RunConfig = {
    streamingMode: StreamingMode.BIDI,
    responseModalities: ["AUDIO", "TEXT"] as any,
    inputAudioTranscription: { languageCodes: ["en-US"] },
    outputAudioTranscription: { languageCodes: ["en-US"] },
    maxLlmCalls: 30
};


(async () => {
    const liveRequestQueue = new LiveRequestQueue();
    // session-specific and stateful
    // thread-safe async queue buffers user messages for orderly processing (text context, audio blobs, activity signals)
    // DO NOT REUSE

    try {
        console.log(`[Lifecycle] Session Active: ${session.id}. Ready for live research.`);

        // Initial Realtime Pulse (Input side)
        const sendInitialAudio = () => {
            const initialBlob: Blob = {
                mimeType: "audio/pcm; rate=16000",
                data: ""
            };
            liveRequestQueue.sendRealtime(initialBlob);
        };
        sendInitialAudio();

        const stream = runner.runAsync({
            userId,
            sessionId,
            runConfig,
            // @ts-ignore - liveRequestQueue is the core mechanism for BIDI interaction
            liveRequestQueue: liveRequestQueue,
            newMessage: { parts: [{ text: "System Initialized. I am ready to begin the research session." }] }
        });

        // Event Processing Loop (Downstream side)
        for await (const event of stream) {
            // Handle Synthesis Output
            if (event.content?.parts) {
                for (const part of event.content.parts) {
                    if (part.text) console.log(`[Agent Synthesis]: ${part.text}`);
                }
            }

            // Handle Real-time Transcriptions
            if (event.inputTranscription?.text) {
                console.log(`[User Audio]: ${event.inputTranscription.text}`);

                // Example Activity Signal: Handle exit
                if (event.inputTranscription.text.toLowerCase().includes("terminate session")) {
                    break;
                }
            }

            // Handle Interruptions or Turn Completions
            if (event.interrupted) {
                console.warn("[Lifecycle] Synthesis interrupted by user activity.");
            }
        }
    } catch (err) {
        console.error("[Runtime Error] Session crashed:", err);
    } finally {
        // --- PHASE 4: TERMINATION & CLEANUP ---
        console.log("[Lifecycle] Closing live queue and terminating session.");
        liveRequestQueue.close();
        process.exit(0);
    }
})();
