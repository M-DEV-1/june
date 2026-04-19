export type OraStatus = 'INIT' | 'CONNECTING' | 'LISTENING' | 'PROCESSING' | 'SPEAKING' | 'ERROR';

export type AppMessage = {
    role: 'user' | 'agent' | 'system';
    text: string;
    partial?: boolean;
};

export type WaveformMode = 1 | 2 | 3 | 4;

export type PaletteName = 'cyberpunk' | 'neon' | 'aurora' | 'sunset';
