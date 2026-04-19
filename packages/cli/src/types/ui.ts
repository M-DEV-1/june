import { OraStatus, AppMessage } from '@ora/core';

export type ExtendedOraStatus = OraStatus | 'BOOTING' | 'IDLE';

export interface TraceEntry {
    id: string;
    agent: 'orch' | 'rsrch' | 'shell' | 'code';
    tool?: string;
    thought?: string;
    args?: Record<string, unknown>;
    status: 'pending' | 'success' | 'error';
    durationMs?: number;
    timestamp: number;
}

export interface MemoryEvent {
    type: 'compaction';
    tokensSaved: number;
    timestamp: number;
}

export interface AppState {
    status: ExtendedOraStatus;
    
    // Audio levels
    micLevel: number;
    spkLevel: number;
    analyserData: number[];
    
    // Visuals
    waveformMode: 1 | 2 | 3 | 4;
    palette: 'warm' | 'cool' | 'neon';
    
    // Session stats
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
