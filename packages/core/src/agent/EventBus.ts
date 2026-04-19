import { EventEmitter } from 'node:events';
import { OraStatus } from '../types/index.js';

export interface ToolStartData {
    agent: string;
    tool: string;
    args: Record<string, unknown>;
    thought?: string;
}

export interface ToolEndData {
    agent: string;
    tool: string;
    durationMs: number;
    result: unknown;
}

export interface AudioLevelData {
    source: 'mic' | 'speaker';
    level: number;
}

export interface ErrorData {
    message: string;
    recoverable: boolean;
}

export interface CompactionData {
    tokensSaved: number;
}

export interface TranscriptData {
    role: 'user' | 'agent' | 'system';
    text: string;
    partial: boolean;
    timestamp: number;
}

export interface RawAdkEvent {
    serverContent?: {
        modelTurn?: {
            parts?: Array<{
                metadata?: {
                    cacheHit?: boolean;
                };
            }>;
        };
    };
}

export type OraEvent =
  | { type: 'status'; status: OraStatus }
  | ({ type: 'transcript' } & TranscriptData)
  | ({ type: 'tool:start' } & ToolStartData)
  | ({ type: 'tool:end' } & ToolEndData)
  | ({ type: 'audio:level' } & AudioLevelData)
  | ({ type: 'error' } & ErrorData)
  | { type: 'session:end' }
  | ({ type: 'memory:compaction' } & CompactionData)
  | ({ type: 'all' } & RawAdkEvent);

export type EventCallback<T extends OraEvent['type']> = (event: Extract<OraEvent, { type: T }>) => void;

export class EventBus extends EventEmitter {
    emitEvent(event: OraEvent) {
        this.emit(event.type, event);
        if (event.type !== 'all') {
            this.emit('all', { ...event, type: 'all' });
        }
    }

    onEvent<T extends OraEvent['type']>(
        type: T,
        handler: EventCallback<T>
    ) {
        this.on(type, handler as (...args: unknown[]) => void);
        return () => {
            this.off(type, handler as (...args: unknown[]) => void);
        };
    }

    // Overload for cleaner React usage
    subscribe<T extends OraEvent['type']>(
        type: T,
        handler: EventCallback<T>
    ) {
        return this.onEvent(type, handler);
    }
}

export const eventBus = new EventBus();
