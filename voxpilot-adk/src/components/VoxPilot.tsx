import React, { useState, useEffect } from 'react';
import { Box, useInput, useApp } from 'ink';
import type { VoxPilotStatus, AppMessage, WaveformMode, PaletteName } from 'voxpilot-shared/types/index.js';
import Banner from 'voxpilot-shared/components/Banner.js';
import Onboarding from 'voxpilot-shared/components/Onboarding.js';
import Waveform from 'voxpilot-shared/components/Waveform.js';
import StatusBar from 'voxpilot-shared/components/StatusBar.js';
import Transcript from 'voxpilot-shared/components/Transcript.js';
import Footer from 'voxpilot-shared/components/Footer.js';
import { runVoxPilotSession } from '../agent.js';

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

		async function startSession() {
			try {
				const stream = runVoxPilotSession(apiKey!);
				setStatus('LISTENING');

				for await (const event of stream as any) {
					if (!isMounted) break;

					// Process events
					if (event.content?.parts) {
						const text = event.content.parts.map((p: any) => p.text).join('');
						if (text) {
							setMessages(prev => [...prev, { role: 'agent', text }]);
							setStatus('SPEAKING');
						}
					}

					if (event.inputTranscription?.text) {
						setMessages(prev => [...prev, { role: 'user', text: event.inputTranscription.text }]);
					}

					if (event.toolCall) {
						setActiveTools(prev => [...prev, event.toolCall.name]);
						setStatus('PROCESSING');
					}

					if (event.toolResponse) {
						setActiveTools(prev => prev.filter(t => t !== event.toolResponse.name));
						setStatus('LISTENING');
					}

					if (event.interrupted) {
						setStatus('LISTENING');
					}

					if (event.serverContent?.modelTurn?.parts?.some((p: any) => p.inlineData)) {
						setStatus('SPEAKING');
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
