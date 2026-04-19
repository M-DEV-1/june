import type { Readable } from 'node:stream';

declare global {
    namespace NodeJS {
        interface ProcessEnv {
            GEMINI_API_KEY?: string;
            [key: string]: string | undefined;
        }
    }
}

declare module 'node-record-lpcm16' {
    export interface RecordOptions {
        sampleRate?: number;
        channels?: number;
        compress?: boolean;
        threshold?: number;
        thresholdStart?: number;
        thresholdEnd?: number;
        silence?: string;
        verbose?: boolean;
        recordProgram?: 'rec' | 'sox' | 'arecord' | 'ffmpeg';
        device?: string;
    }

    export function record(options?: RecordOptions): {
        stream(): Readable;
        pause(): void;
        resume(): void;
        stop(): void;
    };
}

export {};
