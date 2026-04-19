import { describe, it, expect } from 'vitest';
import { SessionManager } from './SessionManager.js';
import { LlmAgent } from '@google/adk';

describe('SessionManager', () => {
    const mockAgent = new LlmAgent({
        name: 'test_agent',
        model: 'gemini-1.5-flash',
        instruction: 'test'
    });

    it('should initialize correctly', () => {
        const sm = new SessionManager(mockAgent);
        expect(sm).toBeDefined();
    });

    it('should toggle mute', () => {
        const sm = new SessionManager(mockAgent);
        expect(sm.toggleMute()).toBe(true);
        expect(sm.toggleMute()).toBe(false);
    });
});
