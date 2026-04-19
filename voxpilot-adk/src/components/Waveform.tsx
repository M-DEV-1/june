import React, {useState, useEffect, useRef} from 'react';
import {Box, Text} from 'ink';
import { audioLevels } from '../utils/audio.js';

interface WaveformProps {
	mode: number; // 1: Oscilloscope, 2: Bars, 3: Radial, 4: Particles
	palette: string;
	isProcessing?: boolean;
}

const PALETTES: Record<string, string[]> = {
	cyberpunk: ['#ff00ff', '#00ffff', '#ffff00', '#ff0000'],
	neon: ['#39ff14', '#bcff00', '#00f9ff', '#ff00ff'],
	aurora: ['#00d4ff', '#00ffaa', '#88ff00', '#ff00ff'],
	sunset: ['#ff4e50', '#fc913a', '#f9d423', '#ede574']
};

const Waveform: React.FC<WaveformProps> = ({mode, palette, isProcessing}) => {
	const colors = (PALETTES[palette] || PALETTES['cyberpunk']) as string[];
    const [phase, setPhase] = useState(0);
    const [localData, setLocalData] = useState<number[]>(new Array(20).fill(0));
    const lastUpdate = useRef(Date.now());

    useEffect(() => {
        const timer = setInterval(() => {
            const now = Date.now();
            const delta = (now - lastUpdate.current) / 1000;
            lastUpdate.current = now;

            setPhase(p => (p + delta * 5) % (Math.PI * 2));
            
            setLocalData(prev => {
                const level = Math.max(audioLevels.recorder, audioLevels.player);
                const newData = [...prev];
                newData.shift();
                
                let target = level;
                if (isProcessing) {
                    target = Math.abs(Math.sin(now / 100)) * 0.5 + 0.2;
                } else if (level < 0.01) {
                    target = Math.random() * 0.05; // Idle noise
                }

                newData.push(target);
                return newData;
            });
        }, 50); // ~20 FPS for stability
        return () => clearInterval(timer);
    }, [isProcessing]);

	const renderOscilloscope = () => {
		const width = 20; // Matches sidebar constraints better
		const height = 8;
		let lines = [];
		for (let y = 0; y < height; y++) {
			let row = '';
			for (let x = 0; x < width; x++) {
				const intensity = localData[Math.floor((x / width) * localData.length)] || 0;
				const mid = height / 2;
				const wave = Math.sin(x * 0.5 + phase) * intensity * (height / 2);
				const targetY = Math.round(mid + wave);
				
				if (y === targetY) {
					row += '█';
				} else if (Math.abs(y - targetY) < 1.5) {
					row += '▓';
				} else {
					row += ' ';
				}
			}
			lines.push(<Text key={y} color={colors[y % colors.length] || 'white'}>{row}</Text>);
		}
		return <Box flexDirection="column">{lines}</Box>;
	};

	const renderBars = () => {
		const height = 10;
		return (
			<Box height={height} alignItems="flex-end" width="100%" justifyContent="center">
				{localData.slice(-15).map((val, i) => {
					const barHeight = Math.max(1, Math.round(val * height));
					const content = '█'.repeat(barHeight).split('').map((char, index) => (
						<Text key={index} color={colors[index % colors.length] || 'white'}>{char}</Text>
					));
					return (
						<Box key={i} flexDirection="column-reverse" marginRight={1}>
							{content}
						</Box>
					);
				})}
			</Box>
		);
	};

	const renderRadial = () => {
        const level = Math.max(...localData.slice(-5));
        const size = Math.floor(level * 10) + 2;
        return (
            <Box height={10} width="100%" justifyContent="center" alignItems="center" flexDirection="column">
                <Text color={colors[0] || 'white'}>
                    {'⬤'.repeat(size)}
                </Text>
                <Text color={colors[1] || 'white'}>
                    {'⬤'.repeat(Math.max(0, size - 2))}
                </Text>
            </Box>
        );
	};

    const renderParticles = () => {
        return (
            <Box flexDirection="column" alignItems="center" height={10} width="100%">
                {localData.slice(-8).map((val, i) => (
                    <Box key={i} marginLeft={Math.round(Math.sin(i + phase) * val * 10)}>
                        <Text color={colors[i % colors.length] || 'white'}>{'✧'.repeat(Math.round(val * 10) + 1)}</Text>
                    </Box>
                ))}
            </Box>
        );
    }

	return (
		<Box 
            borderStyle="single" 
            borderColor="gray" 
            paddingX={1} 
            paddingY={1} 
            minHeight={12} 
            width="100%"
            justifyContent="center"
            alignItems="center"
        >
			{mode === 1 && renderOscilloscope()}
			{mode === 2 && renderBars()}
			{mode === 3 && renderRadial()}
			{mode === 4 && renderParticles()}
		</Box>
	);
};

export default Waveform;
