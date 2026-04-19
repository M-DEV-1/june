import React, { useState, useEffect, useCallback } from 'react';
import { Box, useInput, useApp, Text } from 'ink';
import Gradient from 'ink-gradient';
import figlet from 'figlet';
import { 
    OraStatus, 
    AppMessage, 
    eventBus, 
    SessionManager, 
    orchestratorAgent, 
    Doctor,
    stripAnsi
} from '@ora/core';
import Onboarding from './Onboarding.js';
import Waveform from './Waveform.js';
import StatusBar from './StatusBar.js';
import Transcript from './Transcript.js';
import AgentTrace, { TraceEntry } from './AgentTrace.js';

const Ora: React.FC = () => {
	const { exit } = useApp();
	const [status, setStatus] = useState<OraStatus>('INIT');
	const [apiKey, setApiKey] = useState<string | null>(null);
	const [messages, setMessages] = useState<AppMessage[]>([]);
	const [traces, setTraces] = useState<TraceEntry[]>([]);
	const [fps, setFps] = useState(0);
	const [latency, setLatency] = useState(0);
	const [tokens, setTokens] = useState(0);
    const [cacheHit, setCacheHit] = useState(false);
	const [terminalTooSmall, setTerminalTooSmall] = useState(false);
    const [sessionManager] = useState(() => new SessionManager(orchestratorAgent));
    const [banner, setBanner] = useState('');

    useEffect(() => {
        setBanner(figlet.textSync('ORA', { font: 'Slant' }));
    }, []);

    const checkDeps = useCallback(async () => {
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
        }
        if (key.ctrl && input === 'm') {
            sessionManager.toggleMute();
        }
	});

    useEffect(() => {
        const checkSize = () => {
            setTerminalTooSmall(process.stdout.columns < 100 || process.stdout.rows < 20);
        };
        checkSize();
        process.stdout.on('resize', checkSize);
        return () => { process.stdout.off('resize', checkSize); };
    }, []);

    useEffect(() => {
        const unsubStatus = eventBus.subscribe('status', (e: any) => setStatus(e.status));
        const unsubError = eventBus.subscribe('error', (e: any) => {
            setMessages(prev => [...prev, { role: 'system', text: `ERROR: ${e.message}` }]);
        });
        const unsubTranscript = eventBus.subscribe('transcript', (e: any) => {
            setMessages(prev => {
                const last = prev[prev.length - 1];
                if (last && last.role === e.role && e.role === 'agent') {
                    return [...prev.slice(0, -1), { role: e.role, text: last.text + e.text, partial: e.partial }];
                }
                return [...prev, { role: e.role, text: e.text, partial: e.partial }];
            });
        });
        const unsubToolStart = eventBus.subscribe('tool:start', (e: any) => {
            const roleMap: Record<string, any> = {
                'ora': 'orchestrator',
                'researcher': 'researcher',
                'librarian': 'librarian'
            };
            setTraces(prev => [...prev, {
                agent: e.agent,
                role: roleMap[e.agent] || 'orchestrator',
                tool: e.tool,
                thought: e.thought,
                args: e.args,
                status: 'pending',
                timestamp: Date.now()
            }]);
        });
        const unsubToolEnd = eventBus.subscribe('tool:end', (e: any) => {
            setTraces(prev => prev.map(t => 
                t.tool === e.tool && t.status === 'pending' 
                ? { ...t, status: 'success', durationMs: e.durationMs } 
                : t
            ));
        });

        const unsubCompaction = eventBus.subscribe('memory:compaction', (e: any) => {
            setMessages(prev => [...prev, { role: 'system', text: `⚡ Memory compacted: saved ~${e.tokensSaved} tokens` }]);
        });

        // ADK 0.5.0: Listen for server metadata to track caching
        const unsubAll = eventBus.subscribe('all' as any, (e: any) => {
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
            if (!(await Doctor.hasRequiredDeps())) {
                setMessages(prev => [...prev, { role: 'system', text: 'CRITICAL: Audio engine (ffmpeg/ffplay) missing. See setup guide.' }]);
                setStatus('ERROR');
                return;
            }

            const start = Date.now();
            sessionManager.start(apiKey).then(() => {
                setLatency(Date.now() - start);
            });
        }
        
        run();
	}, [status, apiKey]);

	useEffect(() => {
		if (status === 'INIT' || status === 'CONNECTING') return;
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

	const handleOnboardingComplete = (key: string) => {
		setApiKey(key);
		setStatus('CONNECTING');
	};

    if (terminalTooSmall) {
        return (
            <Box flexDirection="column" alignItems="center" justifyContent="center" width="100%" height={10}>
                <Text color="red" bold underline>TERMINAL TOO SMALL</Text>
                <Text color="yellow">Please expand your terminal for the full dashboard experience.</Text>
                <Text color="gray">Current: {process.stdout.columns}x{process.stdout.rows} | Required: 100x20</Text>
            </Box>
        );
    }

	if (status === 'INIT') {
        return (
            <Box flexDirection="column" alignItems="center" padding={1} width="100%">
                <Gradient name="atlas">
                    <Text>{banner}</Text>
                </Gradient>
                <Onboarding 
                    onComplete={handleOnboardingComplete} 
                    checkDependencies={checkDeps as any}
                />
            </Box>
        );
    }

	return (
		<Box flexDirection="column" width="100%" height="100%" paddingX={2}>
            {/* Header Area */}
            <Box justifyContent="space-between" alignItems="flex-end" marginBottom={1}>
                <Box flexDirection="column">
                    <Gradient name="atlas">
                        <Text>{banner}</Text>
                    </Gradient>
                    <Text color="gray" dimColor>  NEURAL INTERFACE [ADK-DRIVEN] · v2.0</Text>
                </Box>
                <Box flexDirection="column" alignItems="flex-end">
                    <StatusBar status={status} fps={fps} latency={latency} tokens={tokens} cacheHit={cacheHit} />
                </Box>
            </Box>

            <Box flexDirection="row" flexGrow={1}>
                {/* Left Side: Waveform & Traces */}
                <Box flexDirection="column" width={38} marginRight={2}>
                    <Waveform isProcessing={status === 'PROCESSING'} />
                    <Box marginTop={1} flexGrow={1}>
                        <AgentTrace traces={traces} />
                    </Box>
                    <Box marginTop={1} paddingX={1} borderStyle="round" borderColor="gray">
                         <Text color="gray" dimColor>[^M] MUTE  [^R] RESET  [^C] QUIT</Text>
                    </Box>
                </Box>

                {/* Right Side: Primary Transcript */}
                <Box flexDirection="column" flexGrow={1} borderStyle="round" borderColor="cyan" paddingX={1}>
                    <Box borderStyle="single" borderTop={false} borderLeft={false} borderRight={false} marginBottom={1}>
                        <Text bold color="cyan">COMMUNICATION UPLINK</Text>
                    </Box>
                    <Transcript messages={messages} />
                </Box>
            </Box>
        </Box>
	);
};

export default Ora;
