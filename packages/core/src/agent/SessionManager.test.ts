import { describe, it, expect, vi, beforeEach } from 'vitest';
import { SessionManager } from './SessionManager.js';
import { LlmAgent } from '@google/adk';
import { eventBus } from './EventBus.js';

vi.mock('@google/adk', () => {
    return {
        LlmAgent: vi.fn().mockImplementation(() => ({ 
            name: 'ora', 
            instruction: 'test',
            runAsync: vi.fn().mockReturnValue((async function* () {
                yield { content: { parts: [{ text: 'Hello' }] } };
            })())
        })),
        InMemorySessionService: vi.fn().mockImplementation(() => ({
            createSession: vi.fn().mockResolvedValue({ id: 'test-session' }),
            getSession: vi.fn().mockResolvedValue({ id: 'test-session', events: [] })
        })),
        Runner: vi.fn().mockImplementation(() => ({
            runLive: vi.fn().mockReturnValue((async function* () {
                yield { content: { parts: [{ text: 'Hello' }] } };
            })()),
            runAsync: vi.fn().mockReturnValue((async function* () {
                yield { content: { parts: [{ text: 'Hello' }] } };
            })()),
            pluginManager: {
                runBeforeModelCallback: vi.fn().mockResolvedValue(null),
                runAfterModelCallback: vi.fn().mockResolvedValue(null)
            }
        })),
        InMemoryRunner: vi.fn().mockImplementation(() => ({
            runEphemeral: vi.fn().mockReturnValue((async function* () {
                yield { content: { parts: [{ text: 'Compressed summary' }] } };
            })())
        })),
        LiveRequestQueue: vi.fn().mockImplementation(() => ({
            sendRealtime: vi.fn(),
            sendContent: vi.fn(),
            close: vi.fn()
        })),
        StreamingMode: { BIDI: 'bidi' },
        InvocationContext: vi.fn().mockImplementation((params) => params),
        newInvocationContextId: vi.fn().mockReturnValue('test-invocation-id')
    };
});

vi.mock('../audio/MicCapture.js', () => ({
    micCapture: { start: vi.fn().mockReturnValue({ on: vi.fn() }), stop: vi.fn() }
}));

vi.mock('../audio/SpeakerOutput.js', () => ({
    speakerOutput: { addChunk: vi.fn(), stop: vi.fn(), interrupt: vi.fn() }
}));

describe('SessionManager', () => {
    let sm: SessionManager;
    const mockAgent = new LlmAgent({ name: 'ora' }) as any;

    beforeEach(() => {
        sm = new SessionManager(mockAgent);
        vi.clearAllMocks();
    });

    it('should toggle mute', () => {
        expect(sm.toggleMute()).toBe(true);
        expect(sm.toggleMute()).toBe(false);
    });

    it('should start session and emit status', async () => {
        const emitSpy = vi.spyOn(eventBus, 'emitEvent');
        // We don't await start because it loops indefinitely in real use
        sm.start('test-key');
        
        await new Promise(resolve => setTimeout(resolve, 50));
        expect(emitSpy).toHaveBeenCalledWith(expect.objectContaining({ type: 'status', status: 'LISTENING' }));
    });
});
