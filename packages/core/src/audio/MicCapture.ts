import { spawn, execSync } from 'node:child_process';
import { PassThrough } from 'node:stream';
import { ffmpegBin } from '../config/paths.js';
import { eventBus } from '../agent/EventBus.js';

export class MicCapture {
    private process: any = null;
    private outputStream: PassThrough | null = null;
    private meterInterval: NodeJS.Timeout | null = null;
    private currentLevel: number = 0;

    private getWindowsAudioDevice(): string {
        let output: any = '';
        try {
            output = execSync(`${ffmpegBin} -list_devices true -f dshow -i dummy 2>&1`, { encoding: 'utf8', stdio: 'pipe' });
        } catch (error: any) {
            output = error.stdout || error.stderr || '';
        }
        const outputStr = output ? output.toString() : '';
        const lines = outputStr.split('\n');
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

        let winAudioDevice = process.platform === 'win32' ? this.getWindowsAudioDevice() : 'default';
        const args = process.platform === 'win32' 
            ? ['-f', 'dshow', '-i', `audio=${winAudioDevice}`]
            : process.platform === 'darwin'
            ? ['-f', 'avfoundation', '-i', ':default']
            : ['-f', 'alsa', '-i', 'default'];

        // Sanitize environment to prevent secret leakage
        const { GEMINI_API_KEY, ...safeEnv } = process.env;

        this.process = spawn(ffmpegBin, [
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
        this.process.stdout.pipe(this.outputStream, { end: false });

        this.process.on('exit', (code: number) => {
            if (code !== 0 && code !== null) {
                console.error(`MicCapture process exited with code ${code}. Restarting...`);
                this.process = null;
                setTimeout(() => this.start(), 1000);
            }
        });

        this.process.stdout.on('data', (chunk: Buffer) => {
            let sum = 0;
            for (let i = 0; i < chunk.length; i += 2) {
                const sample = chunk.readInt16LE(i);
                sum += sample * sample;
            }
            const rms = Math.sqrt(sum / (chunk.length / 2));
            this.currentLevel = Math.min(1, rms / 5000);
        });

        this.meterInterval = setInterval(() => {
            eventBus.emitEvent({ type: 'audio:level', source: 'mic', level: this.currentLevel });
        }, 33); // ~30fps

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
