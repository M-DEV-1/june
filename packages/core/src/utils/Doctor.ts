import { spawnSync } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
// @ts-ignore
import ffbinaries from 'ffbinaries';
import { 
    ffmpegBin, 
    ffplayBin, 
    localFfmpeg, 
    localFfplay, 
    binDir 
} from '../config/paths.js';

export interface DependencyStatus {
    name: string;
    found: boolean;
    version?: string;
    error?: string;
    instruction?: string;
}

export class Doctor {
    static checkBinary(binName: string, localPath?: string): DependencyStatus {
        const tryPaths = localPath ? [localPath, binName] : [binName];
        const tryFlags = ['-version', '--version', '-v'];
        let lastError = null;

        for (const p of tryPaths) {
            for (const flag of tryFlags) {
                try {
                    // Using spawnSync for safer command execution without shell interpretation
                    const result = spawnSync(p, [flag], {
                        encoding: 'utf8',
                        stdio: 'pipe'
                    });
                    
                    if (result.status === 0 || result.stdout) {
                        const output = result.stdout || result.stderr || '';
                        const firstLine = output.split('\n')[0];
                        return {
                            name: binName,
                            found: true,
                            version: firstLine?.trim()
                        };
                    }
                    lastError = new Error(result.stderr || 'Execution failed');
                } catch (error: any) {
                    lastError = error;
                }
            }
        }

        let instruction = '';
        if (process.platform === 'win32') {
            instruction = `Install via Chocolatey: 'choco install ffmpeg' or download from ffmpeg.org`;
        } else if (process.platform === 'darwin') {
            instruction = `Install via Homebrew: 'brew install ffmpeg'`;
        } else {
            instruction = `Install via your package manager: e.g., 'sudo apt install ffmpeg'`;
        }

        return {
            name: binName,
            found: false,
            error: lastError?.message || 'Binary not found',
            instruction
        };
    }

    static async checkAll(): Promise<DependencyStatus[]> {
        return [
            this.checkBinary(ffmpegBin, localFfmpeg),
            this.checkBinary(ffplayBin, localFfplay)
        ];
    }

    static async hasRequiredDeps(): Promise<boolean> {
        const deps = await this.checkAll();
        return deps.every(d => d.found);
    }

    /**
     * Attempts to download missing binaries if possible.
     * This is blocking during the boot phase to ensure stability.
     */
    static async provisionBinaries(): Promise<boolean> {
        return new Promise((resolve) => {
            if (!fs.existsSync(binDir)) {
                fs.mkdirSync(binDir, { recursive: true });
            }

            const missing = [];
            if (!fs.existsSync(localFfmpeg)) missing.push('ffmpeg');
            if (!fs.existsSync(localFfplay)) missing.push('ffplay');

            if (missing.length === 0) return resolve(true);

            ffbinaries.downloadBinaries(missing, { destination: binDir, quiet: true }, (err: any) => {
                if (err) {
                    resolve(false);
                } else {
                    resolve(true);
                }
            });
        });
    }
}
