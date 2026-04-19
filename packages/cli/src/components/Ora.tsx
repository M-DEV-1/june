import React, { useState, useEffect, useCallback } from 'react';
import { Box, useInput, useApp, Text } from 'ink';
import figlet from 'figlet';
import { 
    OraStatus, 
    AppMessage, 
    eventBus, 
    SessionManager, 
    orchestratorAgent, 
    Doctor,
    type DependencyStatus
} from '@ora/core';
import Header from './Header.js';
import LeftPanel from './LeftPanel.js';
import RightPanel from './RightPanel.js';
import BootScreen from './BootScreen.js';
import Onboarding from './Onboarding.js';
import Footer from './Footer.js';
import { COLORS } from '../themes/index.js';
import { AppState, TraceEntry } from '../types/ui.js';

const Ora: React.FC = () => {
    const { exit } = useApp();
    const [status, setStatus] = useState<OraStatus>('BOOTING');
    const [apiKey, setApiKey] = useState<string | null>(process.env.GEMINI_API_KEY || null);
    const [messages, setMessages] = useState<AppMessage[]>([]);
    const [traces, setTraces] = useState<TraceEntry[]>([]);
    const [fps, setFps] = useState(0);
    const [latency, setLatency] = useState(0);
    const [tokens, setTokens] = useState(0);
    const [cacheHit, setCacheHit] = useState<boolean | null>(null);
    const [terminalTooSmall, setTerminalTooSmall] = useState(false);
    const [sessionManager] = useState(() => new SessionManager(orchestratorAgent));

    const checkDeps = useCallback(async (): Promise<DependencyStatus[]> => {
        return Doctor.checkAll();
    }, []);

    useInput((input, key) => {
        if (key.ctrl && input === 'c') {
            sessionManager.stop();
            exit();
        }
        if (key.ctrl && input === 'r') {
            sessionManager.reset();
            setMessages([]);
            setTraces([]);
            if (apiKey) setStatus('CONNECTING');
            else setStatus('INIT');
        }
        if (key.ctrl && input === 'm') {
            sessionManager.toggleMute();
        }
    });

    useEffect(() => {
        const checkSize = () => {
            setTerminalTooSmall(process.stdout.columns < 100 || process.stdout.rows < 12);
        };
        checkSize();
        process.stdout.on('resize', checkSize);
        return () => { process.stdout.off('resize', checkSize); };
    }, []);

    useEffect(() => {
        const unsubStatus = eventBus.subscribe('status', (e) => setStatus(e.status));
        
        const unsubError = eventBus.subscribe('error', (e) => {
            setMessages(prev => [...prev, { role: 'system', text: `ERROR: ${e.message}`, timestamp: Date.now() }]);
        });

        const unsubTranscript = eventBus.subscribe('transcript', (e) => {
            setMessages(prev => {
                const last = prev[prev.length - 1];
                if (last && last.role === e.role && e.role === 'agent') {
                    return [...prev.slice(0, -1), { role: e.role, text: last.text + e.text, partial: e.partial, timestamp: last.timestamp }];
                }
                return [...prev, { role: e.role, text: e.text, partial: e.partial, timestamp: e.timestamp }];
            });
        });

        const unsubToolStart = eventBus.subscribe('tool:start', (e) => {
            setTraces(prev => [...prev, {
                id: Math.random().toString(36).substring(7),
                agent: e.agent === 'ora' ? 'orch' : e.agent === 'researcher' ? 'rsrch' : e.agent === 'librarian' ? 'shell' : 'code',
                tool: e.tool,
                thought: e.thought,
                args: e.args,
                status: 'pending',
                timestamp: Date.now()
            }]);
        });

        const unsubToolEnd = eventBus.subscribe('tool:end', (e) => {
            setTraces(prev => prev.map(t => 
                t.tool === e.tool && t.status === 'pending' 
                ? { ...t, status: 'success', durationMs: e.durationMs } 
                : t
            ));
        });

        const unsubCompaction = eventBus.subscribe('memory:compaction', (e) => {
            setMessages(prev => [...prev, { role: 'system', text: `⚡ Memory compacted: saved ~${e.tokensSaved} tokens`, timestamp: Date.now() }]);
        });

        const unsubAll = eventBus.subscribe('all', (e) => {
            if (e.serverContent?.modelTurn?.parts?.[0]?.metadata?.cacheHit) {
                setCacheHit(true);
            }
        });

        return () => {
            unsubStatus();
            unsubError();
            unsubTranscript();
            unsubToolStart();
            unsubToolEnd();
            unsubCompaction();
            unsubAll();
        };
    }, []);

    useEffect(() => {
        if (status !== 'CONNECTING' || !apiKey) return;
        
        async function run() {
            // Attempt to self-heal binaries if missing (non-blocking)
            await Doctor.provisionBinaries();

            const hasDeps = await Doctor.hasRequiredDeps();
            if (!hasDeps) {
                setMessages(prev => [...prev, { role: 'system', text: 'CRITICAL: Audio engine (ffmpeg/ffplay) missing.', timestamp: Date.now() }]);
                setStatus('ERROR');
                return;
            }
            sessionManager.start(apiKey!);
        }
        run();
    }, [status, apiKey, sessionManager]);

    useEffect(() => {
        if (status === 'INIT' || status === 'BOOTING' || status === 'CONNECTING') return;
        let frames = 0;
        let last = Date.now();
        const interval = setInterval(() => {
            frames++;
            const now = Date.now();
            if (now - last >= 1000) {
                setFps(frames);
                frames = 0;
                last = now;
            }
        }, 33);
        return () => clearInterval(interval);
    }, [status]);

    const handleBootComplete = () => {
        if (apiKey) {
            setStatus('CONNECTING');
        } else {
            setStatus('INIT');
        }
    };

    const appState: AppState = {
        status,
        micLevel: 0,
        spkLevel: 0,
        analyserData: [],
        waveformMode: 1,
        palette: 'warm',
        sessionDuration: 0,
        fps,
        latencyMs: latency,
        tokenCount: tokens,
        tokenBudget: 8000,
        memoryTier: 'L1',
        cacheHit,
        transcript: messages,
        agentTrace: traces,
        memoryEvent: null,
        isStreaming: status === 'SPEAKING'
    };

    if (status === 'BOOTING') {
        return <BootScreen onComplete={handleBootComplete} />;
    }

    if (status === 'INIT') {
        return (
            <Box flexDirection="column" alignItems="center" justifyContent="center" width="100%" height="100%">
                {terminalTooSmall && (
                    <Box backgroundColor={COLORS.RED} paddingX={2} marginBottom={1}>
                        <Text color="white">⚠ TERMINAL TOO SMALL (Min: 100x12)</Text>
                    </Box>
                )}
                <Onboarding onComplete={(key) => { setApiKey(key); setStatus('CONNECTING'); }} checkDependencies={checkDeps} />
            </Box>
        );
    }

    return (
        <Box flexDirection="column" width="100%" height="100%" backgroundColor={COLORS.BG_BASE}>
            {terminalTooSmall && (
                <Box backgroundColor={COLORS.RED} paddingX={2}>
                    <Text color="white">⚠ TERMINAL TOO SMALL (Min: 100x12) - UI MAY BE CLIPPED</Text>
                </Box>
            )}
            <Header status={status} />
            <Box flexDirection="row" flexGrow={1}>
                <LeftPanel state={appState} />
                <Box borderRight borderStyle="single" borderColor={COLORS.BORDER} />
                <RightPanel state={appState} />
            </Box>
            <Footer fps={fps} />
        </Box>
    );
};

export default Ora;
