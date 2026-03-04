import type { Readable } from 'node:stream';
import path from 'node:path';
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
import { randomUUID } from 'node:crypto';
import { promises as fs } from 'node:fs';
import { z } from 'zod';
import dotenv from 'dotenv';
// @ts-ignore
import record from 'node-record-lpcm16';

dotenv.config();

/* STEPS ---
 - initialize app (startup)
 - session initialization (once per streaming session)
 - active session (concurrent bidirectional communication)
 - session termination
 - loop to step 3 whenever user does /resume
*/

// all ADK bidirectional components must run in an async context - Session Operations, WebSocket operations, Concurrent tasks

export const APP_NAME = "voxpilot-adk";

const listDirectory = new FunctionTool({
    name: 'list_directory',
    description: 'Lists files in a given directory path.',
    parameters: z.object({
        path: z.string().default('.').describe('The directory path to list.')
    }) as any,
    execute: async (args: any) => {
        const { path: p } = args;
        try {
            const target = path.resolve(process.cwd(), p || '.');
            if (!target.startsWith(process.cwd())) throw new Error("Path traversal blocked.");
            const files = await fs.readdir(target);
            return { files };
        } catch (e: unknown) {
            return { error: `Failed to list directory: ${(e as Error).message}` };
        }
    }
});

const readFile = new FunctionTool({
    name: 'read_research_file',
    description: 'Reads the content of a specific file.',
    parameters: z.object({
        path: z.string().describe('Path to the file to read.')
    }) as any,
    execute: async (args: any) => {
        const { path: p } = args;
        try {
            const target = path.resolve(process.cwd(), p);
            if (!target.startsWith(process.cwd())) throw new Error("Path traversal blocked.");
            const content = await fs.readFile(target, 'utf-8');
            return { content: content.slice(0, 10000) };
        } catch (e: unknown) {
            return { error: `Failed to read file: ${(e as Error).message}` };
        }
    }
});

const saveResearchNotes = new FunctionTool({
    name: 'save_research_notes',
    description: 'Saves synthesized findings or notes to a local file.',
    parameters: z.object({
        path: z.string().describe('File path where notes should be saved.'),
        content: z.string().describe('The synthesized research notes or summary.')
    }) as any,
    execute: async (args: any) => {
        const { path: p, content } = args;
        try {
            const target = path.resolve(process.cwd(), p);
            if (!target.startsWith(process.cwd())) throw new Error("Path traversal blocked.");
            await fs.writeFile(target, content, 'utf-8');
            return { success: true, message: `Notes successfully saved to ${target}` };
        } catch (e: unknown) {
            return { error: `Failed to save notes: ${(e as Error).message}` };
        }
    }
});

export const researchAgent = new LlmAgent({
    name: "research_agent",
    model: "gemini-2.0-flash-exp",
    description: "An expert research assistant with real-time audio analysis.",
    instruction: `
        You are VOXPILOT, a high-performance Research Voice Assistant.
        You have direct access to local files and the web.
        
        Style: Professional, concise, tech-heavy.
    `,
    tools: [
        new GoogleSearchTool(),
        listDirectory,
        readFile,
        saveResearchNotes
    ]
});

const sessionService = new InMemorySessionService();

export async function* runVoxPilotSession(apiKey?: string) {
    if (apiKey) process.env['GEMINI_API_KEY'] = apiKey;

    const userId = randomUUID();
    const sessionId = randomUUID();

    const runner = new Runner({
        appName: APP_NAME,
        agent: researchAgent,
        sessionService: sessionService,
    });

    const runConfig: RunConfig = {
        streamingMode: StreamingMode.BIDI,
        responseModalities: ["AUDIO", "TEXT"] as any,
        inputAudioTranscription: { languageCodes: ["en-US"] },
        outputAudioTranscription: { languageCodes: ["en-US"] },
        maxLlmCalls: 100
    };

    const liveRequestQueue = new LiveRequestQueue();
    // session-specific and stateful
    // thread-safe async queue buffers user messages for orderly processing (text context, audio blobs, activity signals)
    // DO NOT REUSE

    // Initialize real mic capture
    let micStream: Readable | null = null;
    try {
        micStream = record.record({
            sampleRate: 16000,
            threshold: 0,
            verbose: false,
            recordProgram: 'sox', // or 'rec' or 'ffmpeg'
        }).stream();

        micStream?.on('data', (data: Buffer) => {
            liveRequestQueue.sendRealtime({
                mimeType: "audio/pcm; rate=16000",
                data: data.toString('base64')
            });
        });

        micStream?.on('error', (err: unknown) => {
            console.error("Mic Stream Error (Likely missing 'sox'):", (err as Error).message);
            if (micStream) micStream.pause();
            throw err;
        });
    } catch (e: unknown) {
        console.error("Mic capture failed to initialize:", (e as Error).message);
        throw e;
    }

    const stream = runner.runAsync({
        userId,
        sessionId,
        runConfig,
        // @ts-ignore
        liveRequestQueue: liveRequestQueue,
        newMessage: { parts: [{ text: "Initializing VOXPILOT Core." }] }
    });

    try {
        for await (const event of stream) {
            yield event;
        }
    } finally {
        if (micStream) micStream.pause();
        liveRequestQueue.close();
    }
}
