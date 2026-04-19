import { spawn, type ChildProcess } from 'node:child_process';
import { PassThrough } from 'node:stream';
import fs from 'node:fs';
import { ffplayBin, localFfplay } from '../config/paths.js';
import { eventBus } from '../agent/EventBus.js';

export class SpeakerOutput {
    private process: ChildProcess | null = null;
    private inputStream: PassThrough | null = null;
    private meterInterval: NodeJS.Timeout | null = null;
    private currentLevel: number = 0;
    private lastChunkTime: number = 0;
    private readonly IDLE_TIMEOUT = 2000; 
    private chunkQueue: Buffer[] = [];
    private isProcessingQueue = false;
    
    // Jitter Buffer Settings
    private readonly BUFFER_THRESHOLD_MS = 300; // 300ms buffer
    private readonly BYTES_PER_MS = (24000 * 2) / 1000; // 24kHz * 16-bit / 1000ms
    private isBuffering = true;

    private ensureProcess() {
        if (this.process) {
            this.lastChunkTime = Date.now();
            return;
        }

        const binaryToUse = fs.existsSync(localFfplay) ? localFfplay : ffplayBin;
        const { GEMINI_API_KEY, ...safeEnv } = process.env;

        this.process = spawn(binaryToUse, [
            '-f', 's16le', 
            '-ar', '24000', 
            '-ac', '1', 
            '-nodisp', 
            '-autoexit',
            '-'
        ], {
            stdio: ['pipe', 'ignore', 'ignore'],
            env: safeEnv
        });

        this.inputStream = new PassThrough();
        this.inputStream.pipe(this.process.stdin!);

        this.process.on('exit', () => {
            this.process = null;
            this.inputStream = null;
        });

        this.lastChunkTime = Date.now();
        
        if (!this.meterInterval) {
            this.meterInterval = setInterval(() => {
                if (Date.now() - this.lastChunkTime > this.IDLE_TIMEOUT) {
                    this.stop();
                } else {
                    eventBus.emitEvent({ type: 'audio:level', source: 'speaker', level: this.currentLevel });
                    this.currentLevel *= 0.8;
                }
            }, 33);
        }
    }

    private async processQueue() {
        if (this.isProcessingQueue) return;
        this.isProcessingQueue = true;

        while (this.chunkQueue.length > 0) {
            // Check if we have enough data to stop buffering
            const totalBufferedBytes = this.chunkQueue.reduce((acc, b) => acc + b.length, 0);
            const bufferedMs = totalBufferedBytes / this.BYTES_PER_MS;

            if (this.isBuffering && bufferedMs < this.BUFFER_THRESHOLD_MS) {
                // Not enough data yet, wait for more
                await new Promise(r => setTimeout(r, 50));
                continue;
            }
            
            this.isBuffering = false;
            const chunk = this.chunkQueue.shift()!;
            
            this.ensureProcess();
            if (this.inputStream) {
                const drained = this.inputStream.write(chunk);
                if (!drained) {
                    await new Promise(r => this.inputStream!.once('drain', r));
                }
            }

            // Small delay to prevent tight loop from eating CPU
            await new Promise(r => setTimeout(r, 1));
        }

        this.isProcessingQueue = false;
        if (this.chunkQueue.length === 0) {
            this.isBuffering = true;
        }
    }

    addChunk(chunk: Buffer) {
        let sum = 0;
        for (let i = 0; i < chunk.length; i += 2) {
            if (i + 1 >= chunk.length) break;
            const sample = chunk.readInt16LE(i);
            sum += sample * sample;
        }
        const rms = Math.sqrt(sum / (chunk.length / 2));
        this.currentLevel = Math.max(this.currentLevel, Math.min(1, rms / 5000));
        
        this.chunkQueue.push(chunk);
        this.processQueue().catch(() => {});
    }

    stop() {
        this.chunkQueue = [];
        this.isBuffering = true;
        if (this.process) {
            this.process.kill();
            this.process = null;
        }
        if (this.inputStream) {
            this.inputStream.end();
            this.inputStream = null;
        }
        if (this.meterInterval) {
            clearInterval(this.meterInterval);
            this.meterInterval = null;
        }
        this.currentLevel = 0;
        eventBus.emitEvent({ type: 'audio:level', source: 'speaker', level: 0 });
    }

    interrupt() {
        this.stop(); 
    }
}

export const speakerOutput = new SpeakerOutput();
