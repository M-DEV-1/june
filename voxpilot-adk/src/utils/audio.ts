import { spawn, execSync, exec } from 'node:child_process';
import { Readable } from 'node:stream';
import path from 'node:path';
import os from 'node:os';
import fs from 'node:fs';
// @ts-ignore
import ffbinaries from 'ffbinaries';

const binPath = path.join(os.homedir(), '.voxpilot', 'bin');
const ffmpegPath = path.join(binPath, process.platform === 'win32' ? 'ffmpeg.exe' : 'ffmpeg');

export const audioLevels = {
    recorder: 0,
    player: 0
};

export const ensureBinaries = (): Promise<void> => {
    return new Promise((resolve, reject) => {
        if (!fs.existsSync(binPath)) fs.mkdirSync(binPath, { recursive: true });
        if (fs.existsSync(ffmpegPath)) return resolve();

        console.log('Downloading required audio engine (ffmpeg & ffplay)...');
        ffbinaries.downloadBinaries(['ffmpeg', 'ffplay'], { destination: binPath, quiet: true }, (err: any) => {
            if (err) reject(err);
            else resolve();
        });
    });
};

function getWindowsAudioDevice(): string {
    if (!fs.existsSync(ffmpegPath)) return 'default';
    let output: any = '';
    try {
        output = execSync(`"${ffmpegPath}" -list_devices true -f dshow -i dummy 2>&1`, { encoding: 'utf8', stdio: 'pipe' });
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

export class MicRecorder {
    private process: any = null;
    private stream: Readable | null = null;

    start() {
        if (!fs.existsSync(ffmpegPath)) throw new Error('ffmpeg not found.');
        let winAudioDevice = process.platform === 'win32' ? getWindowsAudioDevice() : 'default';
        const args = process.platform === 'win32' 
            ? ['-f', 'dshow', '-i', `audio=${winAudioDevice}`]
            : process.platform === 'darwin'
            ? ['-f', 'avfoundation', '-i', ':default']
            : ['-f', 'alsa', '-i', 'default'];

        this.process = spawn(ffmpegPath, [...args, '-acodec', 'pcm_s16le', '-ar', '16000', '-ac', '1', '-f', 's16le', '-'], {
            stdio: ['ignore', 'pipe', 'ignore']
        });

        this.stream = this.process.stdout;
        if (this.stream) {
            this.stream.on('data', (chunk: Buffer) => {
                let sum = 0;
                for (let i = 0; i < chunk.length; i += 2) {
                    const sample = chunk.readInt16LE(i);
                    sum += sample * sample;
                }
                audioLevels.recorder = Math.min(1, Math.sqrt(sum / (chunk.length / 2)) / 5000);
            });
        }
        return this.stream!;
    }

    stop() {
        if (this.process) {
            this.process.kill();
            this.process = null;
            audioLevels.recorder = 0;
        }
    }
}

// Simple WAV header generator for Windows/Mac/Linux native players
function createWavHeader(dataLength: number, sampleRate: number = 24000) {
    const buffer = Buffer.alloc(44);
    buffer.write('RIFF', 0);
    buffer.writeUInt32LE(36 + dataLength, 4);
    buffer.write('WAVE', 8);
    buffer.write('fmt ', 12);
    buffer.writeUInt32LE(16, 16);
    buffer.writeUInt16LE(1, 20); // PCM
    buffer.writeUInt16LE(1, 22); // Mono
    buffer.writeUInt32LE(sampleRate, 24);
    buffer.writeUInt32LE(sampleRate * 2, 28);
    buffer.writeUInt16LE(2, 32); // Block align
    buffer.writeUInt16LE(16, 34); // Bits per sample
    buffer.write('data', 36);
    buffer.writeUInt32LE(dataLength, 40);
    return buffer;
}

export class AudioPlayer {
    private tempFile: string;
    private queue: Buffer[] = [];
    private isPlaying = false;

    constructor() {
        this.tempFile = path.join(os.tmpdir(), `voxpilot_${Date.now()}.wav`);
    }

    addChunk(base64Data: string) {
        const chunk = Buffer.from(base64Data, 'base64');
        this.queue.push(chunk);
        
        let sum = 0;
        for (let i = 0; i < chunk.length; i += 2) {
            const sample = chunk.readInt16LE(i);
            sum += sample * sample;
        }
        audioLevels.player = Math.min(1, Math.sqrt(sum / (chunk.length / 2)) / 5000);

        if (!this.isPlaying && this.queue.length >= 3) {
            this.playNext();
        }
    }

    private playNext() {
        if (this.queue.length === 0) {
            this.isPlaying = false;
            audioLevels.player = 0;
            return;
        }

        this.isPlaying = true;
        const currentData = Buffer.concat(this.queue.splice(0, this.queue.length));
        const wavData = Buffer.concat([createWavHeader(currentData.length), currentData]);
        
        fs.writeFileSync(this.tempFile, wavData);

        let command = '';
        if (process.platform === 'win32') {
            command = `powershell -c "(New-Object System.Media.SoundPlayer('${this.tempFile}')).PlaySync()"`;
        } else if (process.platform === 'darwin') {
            command = `afplay ${this.tempFile}`;
        } else {
            command = `aplay ${this.tempFile}`;
        }

        exec(command, () => {
            this.playNext();
        });
    }

    interrupt() {
        this.queue = [];
        // No easy way to kill PlaySync without complex process management
        // but clearing queue stops next chunks
        audioLevels.player = 0;
    }

    stop() {
        this.interrupt();
        if (fs.existsSync(this.tempFile)) try { fs.unlinkSync(this.tempFile); } catch(e) {}
    }
}

export const startMicTest = (onLevel: (level: number) => void) => {
    const recorder = new MicRecorder();
    let stream: Readable;
    try {
        stream = recorder.start();
        stream.on('data', () => onLevel(audioLevels.recorder));
    } catch (e) {}
    return () => recorder.stop();
};

export const checkDependencies = async () => {
    return [{ name: 'ffmpeg (native)', found: fs.existsSync(ffmpegPath) }];
};

export const setupFfmpeg = () => ensureBinaries();
