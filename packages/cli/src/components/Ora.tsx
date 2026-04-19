import React, { useState, useEffect, useCallback } from 'react';
import { Box, useInput, useApp, Text } from 'ink';
import { 
    OraStatus, 
    AppMessage, 
    eventBus, 
    SessionManager, 
    orchestratorAgent, 
    Doctor 
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
	const [tokens] = useState(0);
	const [terminalTooSmall, setTerminalTooSmall] = useState(false);
    const [sessionManager] = useState(() => new SessionManager(orchestratorAgent));

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
            if (apiKey) sessionManager.start(apiKey);
        }
        if (key.ctrl && input === 'm') {
            sessionManager.toggleMute();
        }
	});

    useEffect(() => {
        const checkSize = () => {
            setTerminalTooSmall(process.stdout.columns < 80 || process.stdout.rows < 15);
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

        return () => {
            unsubStatus();
            unsubError();
            unsubTranscript();
            unsubToolStart();
            unsubToolEnd();
            unsubCompaction();
        };
    }, []);

	useEffect(() => {
		if (status !== 'CONNECTING' || !apiKey) return;
        const start = Date.now();
        sessionManager.start(apiKey).then(() => {
            setLatency(Date.now() - start);
        });
	}, [status, apiKey]);

	useEffect(() => {
		if (status === 'INIT') return;
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
                <Text color="yellow">Please expand your terminal to at least 80x15 for the dashboard.</Text>
                <Text color="gray">Current: {process.stdout.columns}x{process.stdout.rows}</Text>
            </Box>
        );
    }

	if (status === 'INIT') {
        return (
            <Box flexDirection="column" alignItems="center" padding={1} width={80}>
                <Onboarding 
                    onComplete={handleOnboardingComplete} 
                    checkDependencies={checkDeps as any}
                />
            </Box>
        );
    }

	return (
		<Box flexDirection="column" width="100%" height="100%">
            {/* Header */}
            <Box paddingX={1} borderStyle="single" borderColor="cyan" justifyContent="space-between">
                <Text bold color="cyan">ORA v2.0</Text>
                <Text color="gray">NEURAL INTERFACE [ADK-DRIVEN]</Text>
            </Box>

            <Box flexDirection="row" flexGrow={1}>
                {/* Left Panel: Status & Diagnostics */}
                <Box flexDirection="column" width={32} borderStyle="single" borderTop={false} paddingX={1}>
                    <StatusBar status={status} fps={fps} latency={latency} tokens={tokens} />
                    <Box marginTop={1}>
                        <Waveform isProcessing={status === 'PROCESSING'} />
                    </Box>
                    <Box marginTop={1} flexGrow={1}>
                        <AgentTrace traces={traces} />
                    </Box>
                    <Box marginTop={1} borderStyle="round" borderColor="gray" paddingX={1}>
                        <Text color="gray" dimColor>
                            [^M] MUTE  [^R] RESET  [^C] QUIT
                        </Text>
                    </Box>
                </Box>

                {/* Main Panel: Transcript */}
                <Box flexDirection="column" flexGrow={1} borderStyle="single" borderTop={false} borderLeft={false} paddingX={1}>
                    <Transcript messages={messages} />
                </Box>
            </Box>
        </Box>
	);
};

export default Ora;
