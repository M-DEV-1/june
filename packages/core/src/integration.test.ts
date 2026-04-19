import { describe, it, expect, vi, beforeEach } from 'vitest';
import { SessionManager } from './agent/SessionManager.js';
import { orchestratorAgent } from './agent/OrchestratorAgent.js';
import { eventBus } from './agent/EventBus.js';

// Deep mock of ADK to simulate full event flow
vi.mock('@google/adk', async (importOriginal) => {
    const actual = await importOriginal() as any;
    return {
        ...actual,
        LlmAgent: vi.fn().mockImplementation((config) => ({
            ...config,
            runAsync: vi.fn().mockImplementation(async function* () {
                // Simulate model producing a thought, then a tool call, then a final response
                yield { content: { parts: [{ thought: 'I should check the files.' }] } };
                yield { toolCall: { name: 'researcher', args: { query: 'test' } } };
                yield { toolResponse: { name: 'researcher', content: 'results' } };
                yield { content: { parts: [{ text: 'Here is the summary.' }] } };
                yield { turnComplete: true };
            })
        })),
        InMemorySessionService: vi.fn().mockImplementation(() => ({
            createSession: vi.fn().mockResolvedValue({ id: 'test-session' }),
            getSession: vi.fn().mockResolvedValue({ id: 'test-session', events: [] })
        })),
        Runner: vi.fn().mockImplementation(() => ({
            runAsync: vi.fn(),
            pluginManager: {
                runBeforeModelCallback: vi.fn().mockResolvedValue(null),
                runAfterModelCallback: vi.fn().mockResolvedValue(null)
            }
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

// Mock audio to avoid process spawning
vi.mock('./audio/MicCapture.js', () => ({
    micCapture: { start: vi.fn().mockReturnValue({ on: vi.fn() }), stop: vi.fn() }
}));

vi.mock('./audio/SpeakerOutput.js', () => ({
    speakerOutput: { addChunk: vi.fn(), stop: vi.fn(), interrupt: vi.fn() }
}));

describe('ORA Integration Loop', () => {
    let sm: SessionManager;

    beforeEach(() => {
        sm = new SessionManager(orchestratorAgent);
        vi.clearAllMocks();
    });

    it('should drive a full agent turn from eventBus perspective', async () => {
        const events: any[] = [];
        eventBus.on('all', (e) => events.push(e));

        // Start session in background
        const startPromise = sm.start('test-api-key');

        // Wait for turn to complete (mock yields fast)
        await new Promise(resolve => setTimeout(resolve, 100));

        // Check for expected event sequence
        const eventTypes = events.map(e => e.type);
        
        expect(eventTypes).toContain('status');
        expect(eventTypes).toContain('transcript');
        expect(eventTypes).toContain('tool:start');
        expect(eventTypes).toContain('tool:end');

        // Verify thought was captured
        const thoughtEvent = events.find(e => e.type === 'tool:start' && e.thought);
        expect(thoughtEvent).toBeDefined();

        // Verify final transcript
        const transcriptEvent = events.find(e => e.type === 'transcript' && e.text === 'Here is the summary.');
        expect(transcriptEvent).toBeDefined();

        sm.stop();
        await startPromise;
    });
});
