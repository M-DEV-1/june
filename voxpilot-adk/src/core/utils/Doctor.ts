import { execSync } from 'node:child_process';
import { ffmpegBin, ffplayBin } from '../config/paths.js';

export interface DependencyStatus {
    name: string;
    found: boolean;
    version?: string;
    error?: string;
    instruction?: string;
}

export class Doctor {
    static checkBinary(binName: string): DependencyStatus {
        try {
            const output = execSync(`${binName} -version`, { encoding: 'utf8', stdio: 'pipe' });
            const firstLine = output.split('\n')[0];
            return {
                name: binName,
                found: true,
                version: firstLine?.trim()
            };
        } catch (error: any) {
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
                error: error.message,
                instruction
            };
        }
    }

    static async checkAll(): Promise<DependencyStatus[]> {
        return [
            this.checkBinary(ffmpegBin),
            this.checkBinary(ffplayBin)
        ];
    }
}
