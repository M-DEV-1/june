import React, { useState, useEffect } from 'react';
import { Box, useInput, useApp, Text } from 'ink';
import type { VoxPilotStatus, AppMessage, WaveformMode, PaletteName } from '../types/index.js';
import Onboarding from './Onboarding.js';
import Waveform from './Waveform.js';
import StatusBar from './StatusBar.js';
import Transcript from './Transcript.js';
import { Doctor } from '../core/utils/Doctor.js';
import { runVoxPilotSession } from '../agent.js';

const VoxPilot: React.FC = () => {
	const { exit } = useApp();
	const [status, setStatus] = useState<VoxPilotStatus>('INIT');
	const [apiKey, setApiKey] = useState<string | null>(null);
	const [mode, setMode] = useState<WaveformMode>(1);
	const [palette, setPalette] = useState<PaletteName>('cyberpunk');
	const [messages, setMessages] = useState<AppMessage[]>([]);
	const [activeTools, setActiveTools] = useState<string[]>([]);
	const [fps, setFps] = useState(0);
    const [latency, setLatency] = useState(0);
    const [terminalTooSmall, setTerminalTooSmall] = useState(false);

    const checkDeps = async () => {
        return Doctor.checkAll();
    };

	useInput((input, key) => {
		if (input === '1') setMode(1);
		if (input === '2') setMode(2);
		if (input === '3') setMode(3);
		if (input === '4') setMode(4);
		if (input === 'c') {
			const palettes: PaletteName[] = ['cyberpunk', 'neon', 'aurora', 'sunset'];
			const next = (palettes.indexOf(palette) + 1) % palettes.length;
			setPalette(palettes[next]!);
		}
		if (key.ctrl && input === 'c') {
			exit();
		}
	});

	useEffect(() => {
		if (status !== 'CONNECTING' || !apiKey) return;

		let isMounted = true;
        const start = Date.now();

		async function startSession() {
			try {
				const stream = runVoxPilotSession(apiKey!);
				setStatus('LISTENING');
                setLatency(Date.now() - start);

				for await (const event of stream as any) {
					if (!isMounted) break;

					if (event.content?.parts) {
						const text = event.content.parts.map((p: any) => p.text).join('');
						if (text) {
							setMessages(prev => {
                                const last = prev[prev.length - 1];
                                if (last?.role === 'agent') {
                                    return [...prev.slice(0, -1), { role: 'agent', text: last.text + text }];
                                }
                                return [...prev, { role: 'agent', text }];
                            });
							setStatus('SPEAKING');
						}
					}

					if (event.inputTranscription?.text) {
						setMessages(prev => [...prev, { role: 'user', text: event.inputTranscription.text }]);
					}

					if (event.toolCall) {
						setActiveTools(prev => [...new Set([...prev, event.toolCall.name])]);
						setStatus('PROCESSING');
					}

					if (event.toolResponse) {
						setActiveTools(prev => prev.filter(t => t !== event.toolResponse.name));
						setStatus('LISTENING');
					}

                    if (event.turnComplete) {
                        setStatus('LISTENING');
                    }

					if (event.interrupted) {
						setStatus('LISTENING');
					}
				}
			} catch (err: any) {
				setMessages(prev => [...prev, { role: 'system', text: `SYSTEM ERROR: ${err.message || 'Unknown error.'}` }]);
				setStatus('ERROR');
			}
		}

		startSession();
		return () => { isMounted = false; };
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
                <Text color="yellow">Please expand your terminal to at least 80x12.</Text>
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
		<Box flexDirection="row" width="100%" height="100%" borderStyle="single">
            <Box flexDirection="column" width={30} height="100%" paddingX={1} borderStyle="single" borderRight={false}>
                <StatusBar status={status} activeTools={activeTools} fps={fps} latency={latency} />
                <Box marginTop={1}>
                    <Waveform 
                        mode={mode} 
                        palette={palette} 
                        isProcessing={status === 'PROCESSING'} 
                    />
                </Box>
                <Box marginTop={1} paddingX={1}>
                    <Text color="gray" dimColor italic>
                        VOXPILOT ADK Engine
                    </Text>
                    <Text color="gray" dimColor>
                        Research Mode Active
                    </Text>
                </Box>
            </Box>
            <Box flexDirection="column" flexGrow={1} height="100%" paddingX={1}>
                <Transcript messages={messages} />
            </Box>
        </Box>
	);
};

export default VoxPilot;
