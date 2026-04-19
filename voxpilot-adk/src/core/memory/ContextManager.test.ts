import { describe, it, expect, vi, beforeEach } from 'vitest';
import { ContextManager } from './ContextManager.js';
import { eventBus } from '../agent/EventBus.js';

vi.mock('@google/adk', () => {
    return {
        LlmAgent: vi.fn(),
        InMemoryRunner: vi.fn().mockImplementation(() => ({
            runEphemeral: vi.fn().mockReturnValue((async function* () {
                yield { content: { parts: [{ text: 'Compressed summary' }] } };
            })())
        }))
    };
});

describe('ContextManager', () => {
    let cm: ContextManager;

    beforeEach(() => {
        cm = new ContextManager();
        vi.clearAllMocks();
    });

    it('should initialize with empty hot window', () => {
        expect(cm.getSystemPromptPrefix()).toBe('');
    });

    it('should ingest turns and track window', async () => {
        await cm.ingestTurn({ role: 'user', text: 'hello' });
        // @ts-ignore
        expect(cm.hotWindow.length).toBe(1);
    });

    it('should emit compaction event on threshold', async () => {
        const emitSpy = vi.spyOn(eventBus, 'emitEvent');
        
        // Fill window with many turns to trigger threshold (6000 tokens)
        for (let i = 0; i < 100; i++) {
            await cm.ingestTurn({ role: 'user', text: 'word '.repeat(50) });
        }

        // The compress() is async and not awaited in ingestTurn, but in tests we can wait
        // for the promise queue or just check spy periodically
        await new Promise(resolve => setTimeout(resolve, 100));
        
        expect(emitSpy).toHaveBeenCalledWith(expect.objectContaining({
            type: 'memory:compaction'
        }));
    });
});
