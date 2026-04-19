import React, { useState, useEffect } from 'react';
import { Box, Text } from 'ink';
import { OraStatus, eventBus } from '@ora/core';

interface StatusBarProps {
    status: OraStatus;
    fps: number;
    latency?: number;
    tokens?: number;
    cacheHit?: boolean;
}

const StatusBar: React.FC<StatusBarProps> = ({ status, fps, latency, tokens }) => {
    const [micLevel, setMicLevel] = useState(0);
    const [spkLevel, setSpkLevel] = useState(0);
    const [cacheHit] = useState(false);

    useEffect(() => {
        const unsubLevel = eventBus.subscribe('audio:level', (e) => {
            if (e.source === 'mic') setMicLevel(e.level);
            if (e.source === 'speaker') setSpkLevel(e.level);
        });

        // Implicitly track cache from status or specific event if ADK provides it
        // For now, we'll listen for a custom event or status shift
        return () => {
            unsubLevel();
        };
    }, []);

    const getStatusColor = () => {
        switch (status) {
            case 'LISTENING': return 'cyan';
            case 'PROCESSING': return 'yellow';
            case 'SPEAKING': return 'magenta';
            case 'ERROR': return 'red';
            default: return 'gray';
        }
    };

    const renderMeter = (level: number, label: string, color: string) => {
        const width = 10;
        const filled = Math.floor(level * width);
        const bar = '█'.repeat(filled) + '░'.repeat(width - filled);
        return (
            <Box>
                <Text color="gray">{label} </Text>
                <Text color={color}>{bar}</Text>
                <Text color="gray"> {(level * 100).toFixed(0).padStart(3)}%</Text>
            </Box>
        );
    };

    return (
        <Box flexDirection="column" width="100%" paddingX={1}>
            <Box justifyContent="space-between">
                <Text bold color={getStatusColor()}>
                    {status === 'LISTENING' ? '◉' : status === 'PROCESSING' ? '○' : '●'} {status}
                </Text>
                <Text color="gray">{fps}fps</Text>
            </Box>

            <Box marginTop={1} flexDirection="column">
                {renderMeter(micLevel, 'MIC', 'cyan')}
                {renderMeter(spkLevel, 'SPK', 'magenta')}
            </Box>

            <Box marginTop={1} justifyContent="space-between">
                <Box>
                    <Text color="gray">LATENCY </Text>
                    <Text color={!latency || latency < 200 ? 'green' : 'yellow'}>{latency || 0}ms</Text>
                </Box>
                <Box>
                    <Text color="gray">CTX </Text>
                    <Text color={cacheHit ? 'green' : 'gray'}>{cacheHit ? 'HIT' : 'MISS'}</Text>
                </Box>
            </Box>

            {tokens !== undefined && (
                <Box marginTop={0}>
                    <Text color="gray">TOKENS </Text>
                    <Text color="blue">{tokens.toLocaleString()}</Text>
                </Box>
            )}
        </Box>
    );
};

export default StatusBar;
