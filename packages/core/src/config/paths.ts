import path from 'node:path';
import os from 'node:os';
import fs from 'node:fs';
import { spawnSync } from 'node:child_process';

// Production-grade path configuration
export const homeDir = path.join(os.homedir(), '.ora');
export const findingsDir = path.join(homeDir, 'findings');
export const sessionsDir = path.join(homeDir, 'sessions');
export const binDir = path.join(homeDir, 'bin');
export const configFile = path.join(homeDir, 'config.json');

// Workspace sandbox directory
export const workspaceDir = path.join(homeDir, 'sandbox'); 

/**
 * Resolves a binary to its absolute path using 'where' (Windows) or 'which' (Unix).
 * Returns the provided fallback if not found or on error.
 */
function resolveSystemBinary(binName: string): string {
    const cmd = process.platform === 'win32' ? 'where.exe' : 'which';
    try {
        const result = spawnSync(cmd, [binName], { encoding: 'utf8' });
        if (result.status === 0 && result.stdout) {
            return result.stdout.split('\n')[0].trim();
        }
    } catch (e) {
        // Silently fall back
    }
    return binName;
}

// System binary names
export const ffmpegBinName = process.platform === 'win32' ? 'ffmpeg.exe' : 'ffmpeg';
export const ffplayBinName = process.platform === 'win32' ? 'ffplay.exe' : 'ffplay';

// Resolved system binaries (absolute paths where possible)
export const ffmpegBin = resolveSystemBinary(ffmpegBinName);
export const ffplayBin = resolveSystemBinary(ffplayBinName);

// Local binary paths
export const localFfmpeg = path.join(binDir, ffmpegBinName);
export const localFfplay = path.join(binDir, ffplayBinName);
