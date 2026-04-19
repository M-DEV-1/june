import { spawnSync } from 'node:child_process';
import { 
    ffmpegBin, 
    ffplayBin, 
} from '../config/paths.js';

export interface DependencyStatus {
    name: string;
    found: boolean;
    version?: string;
    error?: string;
    instruction?: string;
}

export class Doctor {
    static checkBinary(binName: string): DependencyStatus {
        const tryFlags = ['-version', '--version', '-v'];
        let lastError = null;

        for (const flag of tryFlags) {
            try {
                // Using spawnSync for safer command execution without shell interpretation
                const result = spawnSync(binName, [flag], {
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
            this.checkBinary(ffmpegBin),
            // We only strictly need ffmpeg for the new architecture
        ];
    }

    static async hasRequiredDeps(): Promise<boolean> {
        const deps = await this.checkAll();
        return deps.every(d => d.found);
    }

    /**
     * Binary provisioning is now handled by npm install via @ffmpeg-installer/ffmpeg.
     */
    static async provisionBinaries(): Promise<boolean> {
        return true; 
    }
}
