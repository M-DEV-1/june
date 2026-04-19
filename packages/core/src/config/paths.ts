import path from 'node:path';
import os from 'node:os';
import { spawnSync } from 'node:child_process';
// @ts-ignore
import ffmpegInstaller from '@ffmpeg-installer/ffmpeg';

// Production-grade path configuration
export const homeDir = path.join(os.homedir(), '.ora');
export const findingsDir = path.join(homeDir, 'findings');
export const sessionsDir = path.join(homeDir, 'sessions');
export const binDir = path.join(homeDir, 'bin');
export const configFile = path.join(homeDir, 'config.json');

// Workspace sandbox directory
export const workspaceDir = path.join(homeDir, 'sandbox'); 

// Use the bundled ffmpeg from @ffmpeg-installer/ffmpeg
export const ffmpegBin = ffmpegInstaller.path;

// For playback, we'll also use ffmpeg since ffplay might be missing on some systems
export const ffplayBin = ffmpegBin; 

// Local binary paths (backward compatibility)
export const localFfmpeg = ffmpegBin;
export const localFfplay = ffplayBin;

// We can still try to find them on the system as fallback if needed, but the installer path is preferred
export const ffmpegBinName = process.platform === 'win32' ? 'ffmpeg.exe' : 'ffmpeg';
export const ffplayBinName = process.platform === 'win32' ? 'ffplay.exe' : 'ffplay';
