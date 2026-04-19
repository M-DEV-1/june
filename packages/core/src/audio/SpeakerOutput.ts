import { spawn } from 'node:child_process';
import { PassThrough } from 'node:stream';
import { ffplayBin } from '../config/paths.js';
import { eventBus } from '../agent/EventBus.js';

export class SpeakerOutput {
    private process: any = null;
    private inputStream: PassThrough | null = null;
    private meterInterval: NodeJS.Timeout | null = null;
    private currentLevel: number = 0;
    private lastChunkTime: number = 0;
    private readonly IDLE_TIMEOUT = 2000; 
    private chunkQueue: Buffer[] = [];
    private isProcessingQueue = false;

    private ensureProcess() {
        if (this.process) {
            this.lastChunkTime = Date.now();
            return;
        }

        // Sanitize environment to prevent secret leakage
        const { GEMINI_API_KEY, ...safeEnv } = process.env;

        this.process = spawn(ffplayBin, [
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
        this.inputStream.pipe(this.process.stdin);

        this.process.on('exit', (code: number) => {
            if (code !== 0 && code !== null && this.chunkQueue.length > 0) {
                this.process = null;
                this.inputStream = null;
                this.ensureProcess(); // Restart if queue not empty
            } else {
                this.process = null;
                this.inputStream = null;
            }
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
            const chunk = this.chunkQueue.shift()!;
            this.ensureProcess();
            if (this.inputStream) {
                const drained = this.inputStream.write(chunk);
                if (!drained) {
                    await new Promise(r => this.inputStream!.once('drain', r));
                }
            }
            // Small artificial delay for jitter buffering
            await new Promise(r => setTimeout(r, 5));
        }

        this.isProcessingQueue = false;
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
        this.processQueue();
    }

    stop() {
        this.chunkQueue = [];
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
