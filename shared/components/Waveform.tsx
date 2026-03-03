import React, {useState, useEffect} from 'react';
import {Box, Text} from 'ink';

interface WaveformProps {
	mode: number; // 1: Oscilloscope, 2: Bars, 3: Radial, 4: Particles
	palette: string;
	analyserData: number[]; // Array of normalized intensities (0-1)
	isProcessing?: boolean;
}

const PALETTES: Record<string, string[]> = {
	cyberpunk: ['#ff00ff', '#00ffff', '#ffff00', '#ff0000'],
	neon: ['#39ff14', '#bcff00', '#00f9ff', '#ff00ff'],
	aurora: ['#00d4ff', '#00ffaa', '#88ff00', '#ff00ff'],
	sunset: ['#ff4e50', '#fc913a', '#f9d423', '#ede574']
};

const Waveform: React.FC<WaveformProps> = ({mode, palette, analyserData, isProcessing}) => {
	const colors = (PALETTES[palette] || PALETTES['cyberpunk']) as string[];
    const [phase, setPhase] = useState(0);

    useEffect(() => {
        const timer = setInterval(() => {
            setPhase(p => (p + 0.1) % (Math.PI * 2));
        }, 50);
        return () => clearInterval(timer);
    }, []);

	const renderOscilloscope = () => {
		const width = 60;
		const height = 10;
		let lines = [];
		for (let y = 0; y < height; y++) {
			let row = '';
			for (let x = 0; x < width; x++) {
				const intensity = analyserData[Math.floor((x / width) * analyserData.length)] || 0;
				const mid = height / 2;
				const wave = Math.sin(x * 0.2 + phase) * intensity * (height / 2);
				const targetY = Math.round(mid + wave);
				
				if (y === targetY) {
					row += '█';
				} else if (Math.abs(y - targetY) < 1) {
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
		const height = 12;
		return (
			<Box height={height} alignItems="flex-end">
				{analyserData.map((val, i) => {
					const barHeight = Math.round(val * height);
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
        // Simulating radial in TUI is hard, we use a circular expansion/contraction of characters
        const size = Math.floor(Math.max(...analyserData) * 15) + 5;
        const chars = ['@', '#', '*', '.', ' '];
        return (
            <Box height={20} width={40} justifyContent="center" alignItems="center">
                <Text color={colors[0] || 'white'}>
                    {'⬤'.repeat(size / 2)}
                </Text>
            </Box>
        );
	};

    const renderParticles = () => {
        return (
            <Box flexDirection="column" alignItems="center">
                {analyserData.map((val, i) => (
                    <Box key={i} marginLeft={Math.round(Math.sin(i + phase) * val * 20)}>
                        <Text color={colors[i % colors.length] || 'white'}>{'✧'.repeat(Math.round(val * 10))}</Text>
                    </Box>
                ))}
            </Box>
        );
    }

	return (
		<Box 
            borderStyle="double" 
            borderColor="cyan" 
            paddingX={2} 
            paddingY={1} 
            minHeight={15} 
            width={70}
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
