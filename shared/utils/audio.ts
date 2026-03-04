import { execSync } from 'node:child_process';
import ffmpegPath from 'ffmpeg-static';
import path from 'node:path';
// @ts-ignore
import record from 'node-record-lpcm16';

// Ensure ffmpeg-static is in the path
export const setupFfmpeg = () => {
    if (ffmpegPath) {
        const ffmpegDir = path.dirname(ffmpegPath);
        if (process.platform === 'win32') {
            process.env.PATH = `${ffmpegDir};${process.env.PATH}`;
        } else {
            process.env.PATH = `${ffmpegDir}:${process.env.PATH}`;
        }
    }
    return !!ffmpegPath;
};

export const checkDependencies = async () => {
    const deps = [
        { name: 'ffmpeg (bundled)', found: !!ffmpegPath },
        { name: 'sox', found: false }
    ];
    
    try {
        const cmd = process.platform === 'win32' ? 'where sox' : 'which sox';
        execSync(cmd, { stdio: 'ignore' });
        deps[1]!.found = true;
    } catch (e) {}

    return deps;
};

export const startMicTest = (onLevel: (level: number) => void) => {
    setupFfmpeg();
    let micStream: any = null;
    try {
        micStream = record.record({
            sampleRate: 16000,
            threshold: 0,
            verbose: false,
            recordProgram: 'ffmpeg',
        }).stream();

        micStream.on('data', (data: Buffer) => {
            let sum = 0;
            for (let i = 0; i < data.length; i += 2) {
                if (i + 1 < data.length) {
                    const sample = data.readInt16LE(i);
                    sum += sample * sample;
                }
            }
            const rms = Math.sqrt(sum / (data.length / 2));
            const level = Math.min(1, rms / 3000);
            onLevel(level);
        });
    } catch (e) {
        // Silent fail for TUI stability
    }

    return () => {
        if (micStream) {
            try { micStream.pause(); } catch(e) {}
        }
    };
};
