import React, { useState, useEffect } from 'react';
import { Box, Text } from 'ink';
import { eventBus } from '../core/agent/EventBus.js';

interface WaveformProps {
    isProcessing?: boolean;
    color?: string;
}

const Waveform: React.FC<WaveformProps> = ({ isProcessing, color = 'cyan' }) => {
    const [level, setLevel] = useState(0);
    const [history, setHistory] = useState<number[]>(new Array(30).fill(0));
    const [phase, setPhase] = useState(0);

    useEffect(() => {
        const unsubscribe = eventBus.subscribe('audio:level', (e: any) => {
            setLevel(prev => Math.max(prev, e.level));
        });

        const timer = setInterval(() => {
            setHistory(prev => {
                const next = [...prev];
                next.shift();
                
                let target = level;
                if (isProcessing) {
                    target = Math.abs(Math.sin(Date.now() / 200)) * 0.3 + 0.1;
                }
                
                next.push(target);
                setLevel(l => l * 0.5); // Decay current level
                return next;
            });
            setPhase(p => (p + 0.2) % (Math.PI * 2));
        }, 50);

        return () => {
            unsubscribe();
            clearInterval(timer);
        };
    }, [isProcessing]);

    const renderOscilloscope = () => {
        const width = 30;
        const height = 5;
        const mid = Math.floor(height / 2);
        
        const lines = [];
        for (let y = 0; y < height; y++) {
            let row = '';
            for (let x = 0; x < width; x++) {
                const intensity = history[Math.floor((x / width) * history.length)] || 0;
                const wave = Math.sin(x * 0.5 + phase) * intensity * mid;
                const targetY = Math.round(mid + wave);
                
                if (y === targetY) {
                    row += '⎶';
                } else if (Math.abs(y - targetY) < 1) {
                    row += '╌';
                } else {
                    row += ' ';
                }
            }
            lines.push(<Text key={y} color={color} dimColor={y !== mid && !isProcessing}>{row}</Text>);
        }
        return <Box flexDirection="column">{lines}</Box>;
    };

    return (
        <Box 
            paddingX={1} 
            height={7} 
            width="100%"
            justifyContent="center"
            alignItems="center"
            borderStyle="round"
            borderColor="gray"
        >
            {renderOscilloscope()}
        </Box>
    );
};

export default Waveform;
