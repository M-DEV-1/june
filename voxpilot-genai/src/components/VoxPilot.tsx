import React, {useState, useEffect} from 'react';
import {Box, useInput, useApp} from 'ink';
import type { VoxPilotStatus, AppMessage, WaveformMode, PaletteName } from 'voxpilot-shared/types/index.js';
import Banner from 'voxpilot-shared/components/Banner.js';
import Onboarding from 'voxpilot-shared/components/Onboarding.js';
import Waveform from 'voxpilot-shared/components/Waveform.js';
import StatusBar from 'voxpilot-shared/components/StatusBar.js';
import Transcript from 'voxpilot-shared/components/Transcript.js';
import Footer from 'voxpilot-shared/components/Footer.js';
import {runVoxPilotGenAISession} from '../session.js';

const VoxPilot: React.FC = () => {
	const { exit } = useApp();
	const [status, setStatus] = useState<VoxPilotStatus>('INIT');
	const [apiKey, setApiKey] = useState<string | null>(null);
	const [mode, setMode] = useState<WaveformMode>(1);
	const [palette, setPalette] = useState<PaletteName>('cyberpunk');
	const [analyserData, setAnalyserData] = useState<number[]>(new Array(60).fill(0));
	const [messages, setMessages] = useState<AppMessage[]>([]);
	const [activeTools, setActiveTools] = useState<string[]>([]);
	const [fps, setFps] = useState(0);

	// Handle Keyboard Input
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

	// Main Agent Logic
	useEffect(() => {
		if (status !== 'CONNECTING' || !apiKey) return;

		let isMounted = true;

		async function startSession() {
			try {
				const stream = runVoxPilotGenAISession(apiKey!);
				setStatus('LISTENING');

				for await (const message of stream) {
					if (!isMounted) break;
					if (!message) continue;

					// Process GenAI messages
					if (message.serverContent?.modelTurn?.parts) {
						const text = message.serverContent.modelTurn.parts
                            .map((p: any) => p.text)
                            .filter(Boolean)
                            .join('');
						if (text) {
							setMessages(prev => [...prev, {role: 'agent', text}]);
							setStatus('SPEAKING');
						}

                        if (message.serverContent.modelTurn.parts.some((p: any) => p.inlineData)) {
                             setStatus('SPEAKING');
                        }
					}

					if (message.serverContent?.inputTranscription?.text) {
						setMessages(prev => [...prev, {role: 'user', text: message.serverContent.inputTranscription.text}]);
					}

                    if (message.serverContent?.turnComplete) {
                        setStatus('LISTENING');
                    }
				}
			} catch (err) {
				console.error(err);
				setStatus('ERROR');
			}
		}

		startSession();

		return () => { isMounted = false; };
	}, [status, apiKey]);

	// Waveform Animation Loop
	useEffect(() => {
		if (status === 'INIT') return;

		let frameCount = 0;
		let lastTime = Date.now();

		const interval = setInterval(() => {
			setAnalyserData(prev => {
                let newData;
                if (status === 'SPEAKING') {
                    newData = prev.map(() => Math.random() * 0.8 + 0.2);
                } else if (status === 'LISTENING') {
                    newData = prev.map(() => Math.random() * 0.3);
                } else if (status === 'PROCESSING') {
                    newData = prev.map((_, i) => Math.abs(Math.sin((Date.now() / 150) + i * 0.15)) * 0.7);
                } else {
                    newData = prev.map(() => Math.random() * 0.05);
                }
                return newData;
            });

			// Calculate FPS
			frameCount++;
			const now = Date.now();
			if (now - lastTime >= 1000) {
				setFps(frameCount);
				frameCount = 0;
				lastTime = now;
			}
		}, 33);

		return () => clearInterval(interval);
	}, [status]);

	const handleOnboardingComplete = (key: string) => {
		setApiKey(key);
		setStatus('CONNECTING');
	};

	return (
		<Box flexDirection="column" alignItems="center" padding={1} width={80}>
			<Banner />
			
			{status === 'INIT' ? (
				<Onboarding onComplete={handleOnboardingComplete} />
			) : (
				<Box flexDirection="column" alignItems="center">
					<StatusBar status={status} activeTools={activeTools} fps={fps} />
					<Box marginTop={1}>
						<Waveform 
							mode={mode} 
							palette={palette} 
							analyserData={analyserData} 
							isProcessing={status === 'PROCESSING'} 
						/>
					</Box>
					<Transcript messages={messages} />
					<Footer />
				</Box>
			)}
		</Box>
	);
};

export default VoxPilot;
