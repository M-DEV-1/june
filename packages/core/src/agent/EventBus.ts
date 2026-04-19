import { EventEmitter } from 'node:events';

export type OraStatus = 'INIT' | 'LISTENING' | 'PROCESSING' | 'SPEAKING' | 'ERROR';

export type AppMessage = {
    role: 'user' | 'agent' | 'system';
    text: string;
    partial?: boolean;
};

export type OraEvent =
  | { type: 'status'; status: OraStatus }
  | { type: 'transcript'; role: 'user' | 'agent'; text: string; partial: boolean }
  | { type: 'tool:start'; agent: string; tool: string; args: any }
  | { type: 'tool:end'; agent: string; tool: string; durationMs: number; result: any }
  | { type: 'audio:level'; source: 'mic' | 'speaker'; level: number }
  | { type: 'error'; message: string; recoverable: boolean }
  | { type: 'session:end' }
  | { type: 'memory:compaction'; tokensSaved: number };

export class EventBus extends EventEmitter {
    emitEvent(event: OraEvent) {
        this.emit(event.type, event);
        this.emit('all', event);
    }

    onEvent<T extends OraEvent['type']>(
        type: T,
        handler: (event: Extract<OraEvent, { type: T }>) => void
    ) {
        this.on(type, handler as any);
        return () => this.off(type, handler as any);
    }

    on(type: string | symbol, handler: (...args: any[]) => void): this {
        super.on(type, handler);
        return this;
    }

    // Overload for cleaner React usage
    subscribe<T extends OraEvent['type']>(
        type: T,
        handler: (event: Extract<OraEvent, { type: T }>) => void
    ) {
        this.on(type, handler as any);
        return () => {
            this.off(type, handler as any);
        };
    }
}

export const eventBus = new EventBus();
