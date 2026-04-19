import { spawn, spawnSync } from 'node:child_process';
import { PassThrough } from 'node:stream';
import fs from 'node:fs';
import { ffmpegBin, localFfmpeg } from '../config/paths.js';
import { eventBus } from '../agent/EventBus.js';

export class MicCapture {
    private process: any = null;
    private outputStream: PassThrough | null = null;
    private meterInterval: NodeJS.Timeout | null = null;
    private currentLevel: number = 0;

    // AGC Settings
    private readonly TARGET_RMS = 3000;
    private readonly MAX_GAIN = 10.0;
    private readonly MIN_GAIN = 0.5;
    private currentGain = 1.0;

    private getWindowsAudioDevice(): string {
        const binaryToUse = fs.existsSync(localFfmpeg) ? localFfmpeg : ffmpegBin;
        let output: string = '';
        try {
            // Using spawnSync for safer command execution without shell interpretation
            const result = spawnSync(binaryToUse, ['-list_devices', 'true', '-f', 'dshow', '-i', 'dummy'], {
                encoding: 'utf8',
                stdio: 'pipe'
            });
            output = result.stdout || result.stderr || '';
        } catch (error: any) {
            output = error.stdout || error.stderr || '';
        }
        const lines = output.split('\n');
        for (let i = 0; i < lines.length; i++) {
            if (lines[i].includes('(audio)')) {
                if (i + 1 < lines.length && lines[i + 1].includes('Alternative name')) {
                    const match = lines[i + 1].match(/"([^"]+)"/);
                    if (match) return match[1];
                }
                const match = lines[i].match(/"([^"]+)"/);
                if (match) return match[1];
            }
        }
        return 'default';
    }

    start(): PassThrough {
        if (this.process) return this.outputStream!;

        const binaryToUse = fs.existsSync(localFfmpeg) ? localFfmpeg : ffmpegBin;

        let winAudioDevice = process.platform === 'win32' ? this.getWindowsAudioDevice() : 'default';
        const args = process.platform === 'win32' 
            ? ['-f', 'dshow', '-i', `audio=${winAudioDevice}`]
            : process.platform === 'darwin'
            ? ['-f', 'avfoundation', '-i', ':default']
            : ['-f', 'alsa', '-i', 'default'];

        const { GEMINI_API_KEY, ...safeEnv } = process.env;

        this.process = spawn(binaryToUse, [
            ...args, 
            '-acodec', 'pcm_s16le', 
            '-ar', '16000', 
            '-ac', '1', 
            '-f', 's16le', 
            '-'
        ], {
            stdio: ['ignore', 'pipe', 'ignore'],
            env: safeEnv
        });

        if (!this.outputStream) {
            this.outputStream = new PassThrough();
        }

        this.process.on('exit', (code: number) => {
            if (code !== 0 && code !== null) {
                this.process = null;
                setTimeout(() => this.start(), 1000);
            }
        });

        this.process.stdout.on('data', (chunk: Buffer) => {
            // Write original chunk to outputStream for Gemini
            this.outputStream?.write(chunk);

            let sum = 0;
            for (let i = 0; i < chunk.length; i += 2) {
                const sample = chunk.readInt16LE(i);
                sum += sample * sample;
            }
            const rms = Math.sqrt(sum / (chunk.length / 2));

            // Simple AGC logic
            if (rms > 100) { 
                const ratio = this.TARGET_RMS / rms;
                this.currentGain += (ratio - this.currentGain) * 0.05;
                this.currentGain = Math.max(this.MIN_GAIN, Math.min(this.MAX_GAIN, this.currentGain));
            }

            this.currentLevel = Math.min(1, (rms * this.currentGain) / 15000);
        });
        this.meterInterval = setInterval(() => {
            eventBus.emitEvent({ type: 'audio:level', source: 'mic', level: this.currentLevel });
        }, 33); 

        return this.outputStream;
    }

    stop() {
        if (this.process) {
            this.process.kill();
            this.process = null;
        }
        if (this.meterInterval) {
            clearInterval(this.meterInterval);
            this.meterInterval = null;
        }
        this.currentLevel = 0;
        eventBus.emitEvent({ type: 'audio:level', source: 'mic', level: 0 });
    }
}

export const micCapture = new MicCapture();
