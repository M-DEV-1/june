export type OraStatus = 'INIT' | 'BOOTING' | 'CONNECTING' | 'LISTENING' | 'PROCESSING' | 'SPEAKING' | 'IDLE' | 'ERROR';

export type AppMessage = {
    role: 'user' | 'agent' | 'system';
    text: string;
    partial?: boolean;
    timestamp: number;
};

export type WaveformMode = 1 | 2 | 3 | 4;

export type PaletteName = 'cyberpunk' | 'neon' | 'aurora' | 'sunset';
