import { GoogleGenAI } from '@google/genai';
import type { Readable } from 'node:stream';
import { randomUUID } from 'node:crypto';
// import { tools, declarations
import { tools, declarations } from './tools.js';
// @ts-ignore
import record from 'node-record-lpcm16';
import ffmpegPath from 'ffmpeg-static';
import path from 'node:path';

// Ensure ffmpeg-static is in the path
if (ffmpegPath) {
    const ffmpegDir = path.dirname(ffmpegPath);
    if (process.platform === 'win32') {
        process.env.PATH = `${ffmpegDir};${process.env.PATH}`;
    } else {
        process.env.PATH = `${ffmpegDir}:${process.env.PATH}`;
    }
}

export async function* runVoxPilotGenAISession(apiKey: string) {
    const ai = new GoogleGenAI({ apiKey });
    const model = 'gemini-2.0-flash-exp';

    const responseQueue: Record<string, any>[] = [];
    let resolveNext: ((value: Record<string, any>) => void) | null = null;

    const session = await ai.live.connect({
        model,
        callbacks: {
            onmessage: (message) => {
                if (resolveNext) {
                    resolveNext(message);
                    resolveNext = null;
                } else {
                    responseQueue.push(message);
                }
            },
            onerror: (err) => console.error('GenAI Session Error:', err),
            onclose: (e) => console.log('GenAI Session Closed:', e.reason),
            onopen: () => console.log('GenAI Session Opened')
        },
        config: {
            tools: [{ functionDeclarations: declarations }, { googleSearch: {} }],
            responseModalities: ['AUDIO' as any],
            inputAudioTranscription: { languageCodes: ['en-US'] }
        }
    });

    let micStream: Readable | null = null;
    try {
        micStream = record.record({
            sampleRate: 16000,
            threshold: 0,
            verbose: false,
            recordProgram: 'ffmpeg',
        }).stream();

        micStream?.on('data', (data: Buffer) => {
            session.sendRealtimeInput({
                audio: {
                    data: data.toString('base64'),
                    mimeType: 'audio/pcm;rate=16000'
                }
            });
        });
        micStream?.on('error', (err: any) => {
            console.error("Mic Stream Error:", err.message);
            if (micStream) micStream.pause();
        });
    } catch (e: any) {
        console.error("Mic capture failed to initialize:", e.message);
        throw e;
    }

    try {
        while (true) {
            const message = responseQueue.length > 0
                ? responseQueue.shift()
                : await new Promise<Record<string, any>>(resolve => { resolveNext = resolve; });

            if (message?.serverContent?.modelTurn?.parts) {
                for (const part of message.serverContent.modelTurn.parts) {
                    if (part.functionCall) {
                        const { name, args, id } = part.functionCall;
                        const toolFn = tools[name as keyof typeof tools];
                        if (toolFn) {
                            const result = await toolFn(args as any);
                            session.sendClientContent({
                                turns: [{
                                    role: 'user',
                                    parts: [{
                                        functionResponse: {
                                            name,
                                            id,
                                            response: { result }
                                        }
                                    }]
                                }]
                            });
                        }
                    }
                }
            }

            yield message;
        }
    } finally {
        if (micStream) micStream.pause();
        session.close();
    }
}

