import { spawn } from 'node:child_process';
import { PassThrough } from 'node:stream';
import fs from 'node:fs';
import { ffplayBin } from '../config/paths.js';
import { eventBus } from '../agent/EventBus.js';

export class SpeakerOutput {
    private process: any = null;
    private inputStream: PassThrough | null = null;
    private meterInterval: NodeJS.Timeout | null = null;
    private currentLevel: number = 0;
    private lastChunkTime: number = 0;
    private readonly IDLE_TIMEOUT = 1000; // Close ffplay if no audio for 1s

    private ensureProcess() {
        if (this.process) {
            this.lastChunkTime = Date.now();
            return;
        }

        this.process = spawn(ffplayBin, [
            '-f', 's16le', 
            '-ar', '24000', 
            '-ac', '1', 
            '-nodisp', 
            '-autoexit',
            '-'
        ], {
            stdio: ['pipe', 'ignore', 'ignore']
        });

        this.inputStream = new PassThrough();
        this.inputStream.pipe(this.process.stdin);

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
                    // Decay level
                    this.currentLevel *= 0.8;
                }
            }, 33);
        }
    }

    addChunk(chunk: Buffer) {
        this.ensureProcess();
        
        let sum = 0;
        for (let i = 0; i < chunk.length; i += 2) {
            if (i + 1 >= chunk.length) break;
            const sample = chunk.readInt16LE(i);
            sum += sample * sample;
        }
        const rms = Math.sqrt(sum / (chunk.length / 2));
        this.currentLevel = Math.max(this.currentLevel, Math.min(1, rms / 5000));
        
        if (this.inputStream) {
            this.inputStream.write(chunk);
        }
    }

    stop() {
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
        this.stop(); // Killing ffplay is the fastest way to stop audio
    }
}

export const speakerOutput = new SpeakerOutput();
