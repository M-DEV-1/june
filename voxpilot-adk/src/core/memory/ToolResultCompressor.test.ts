import { describe, it, expect, vi } from 'vitest';
import { ToolResultCompressor } from './ToolResultCompressor.js';

vi.mock('@google/adk', () => {
    return {
        LlmAgent: vi.fn(),
        InMemoryRunner: vi.fn().mockImplementation(() => ({
            runEphemeral: vi.fn().mockReturnValue((async function* () {
                yield { content: { parts: [{ text: 'Fact-based summary' }] } };
            })())
        }))
    };
});

describe('ToolResultCompressor', () => {
    it('should return raw if small', async () => {
        const trc = new ToolResultCompressor();
        const result = await trc.compress('test_tool', { data: 'small' }, 'goal');
        expect(result).toBe('{"data":"small"}');
    });

    it('should compress large results', async () => {
        const trc = new ToolResultCompressor();
        const largeData = 'word '.repeat(500);
        const result = await trc.compress('test_tool', { data: largeData }, 'goal');
        expect(result).toBe('Fact-based summary');
    });
});
