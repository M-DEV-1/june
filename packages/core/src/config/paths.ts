import path from 'node:path';
import os from 'node:os';

// Production-grade path configuration
export const homeDir = path.join(os.homedir(), '.ora');
export const findingsDir = path.join(homeDir, 'findings');
export const sessionsDir = path.join(homeDir, 'sessions');
export const binDir = path.join(homeDir, 'bin');
export const configFile = path.join(homeDir, 'config.json');

// Workspace sandbox directory
export const workspaceDir = path.join(homeDir, 'sandbox'); 

// System binary names
export const ffmpegBin = process.platform === 'win32' ? 'ffmpeg.exe' : 'ffmpeg';
export const ffplayBin = process.platform === 'win32' ? 'ffplay.exe' : 'ffplay';

// Local binary paths
export const localFfmpeg = path.join(binDir, ffmpegBin);
export const localFfplay = path.join(binDir, ffplayBin);
