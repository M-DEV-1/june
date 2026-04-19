import path from 'node:path';
import os from 'node:os';

// Production-grade path configuration
export const findingsDir = path.join(os.homedir(), '.voxpilot', 'findings');
export const sessionsDir = path.join(os.homedir(), '.voxpilot', 'sessions');
export const configFile = path.join(os.homedir(), '.voxpilot', 'config.json');

// System binary names
export const ffmpegBin = process.platform === 'win32' ? 'ffmpeg.exe' : 'ffmpeg';
export const ffplayBin = process.platform === 'win32' ? 'ffplay.exe' : 'ffplay';
