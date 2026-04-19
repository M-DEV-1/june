import { OraStatus, AppMessage } from '@ora/core';

export type ExtendedOraStatus = OraStatus | 'BOOTING' | 'IDLE';

export interface TraceEntry {
    id: string;
    agent: 'orch' | 'rsrch' | 'shell' | 'code';
    tool?: string;
    thought?: string;
    args?: any;
    status: 'pending' | 'success' | 'error';
    durationMs?: number;
    startedAt: number;
}

export interface MemoryEvent {
    phase: 'compressing' | 'done';
    savedTokens: number;
    fromTier: string;
    toTier: string;
}

export interface AppState {
    status: ExtendedOraStatus;
    
    // Audio
    micLevel: number;
    spkLevel: number;
    analyserData: number[];
    waveformMode: 1 | 2 | 3 | 4;
    palette: string;
    
    // Session
    sessionDuration: number;
    fps: number;
    latencyMs: number;
    tokenCount: number;
    tokenBudget: number;
    memoryTier: 'L1' | 'L2' | 'L2+L3';
    cacheHit: boolean | null;
    
    // Agent
    transcript: AppMessage[];
    agentTrace: TraceEntry[];
    
    // Memory events
    memoryEvent: MemoryEvent | null;
    
    // Streaming
    isStreaming: boolean;
}
