export type VoxPilotStatus = 'INIT' | 'CONNECTING' | 'LISTENING' | 'PROCESSING' | 'SPEAKING' | 'IDLE' | 'ERROR';

export type AppMessage = {
    role: 'user' | 'agent' | 'system';
    text: string;
};

export type WaveformMode = 1 | 2 | 3 | 4;

export type PaletteName = 'cyberpunk' | 'neon' | 'aurora' | 'sunset';
