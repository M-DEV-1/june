import path from 'node:path';
import os from 'node:os';

// Production-grade path configuration
export const findingsDir = path.join(os.homedir(), '.ora', 'findings');
export const sessionsDir = path.join(os.homedir(), '.ora', 'sessions');
export const configFile = path.join(os.homedir(), '.ora', 'config.json');

// Workspace sandbox directory
export const workspaceDir = path.join(os.homedir(), '.ora', 'sandbox'); 

// System binary names
export const ffmpegBin = process.platform === 'win32' ? 'ffmpeg.exe' : 'ffmpeg';
export const ffplayBin = process.platform === 'win32' ? 'ffplay.exe' : 'ffplay';
