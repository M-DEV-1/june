import { describe, it, expect } from 'vitest';
import { ContextManager } from './ContextManager.js';

describe('ContextManager', () => {
    it('should initialize with empty hot window', () => {
        const cm = new ContextManager();
        expect(cm.getSystemPromptPrefix()).toBe('');
    });

    it('should estimate tokens correctly', () => {
        const cm = new ContextManager();
        // @ts-ignore
        const tokens = cm.estimateTokens('abcd');
        expect(tokens).toBe(1);
    });

    it('should ingest turns', async () => {
        const cm = new ContextManager();
        await cm.ingestTurn({ role: 'user', text: 'hello' });
        // @ts-ignore
        expect(cm.hotWindow.length).toBe(1);
    });
});
