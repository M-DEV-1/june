import React, { useState, useEffect } from 'react';
import { Box, Text } from 'ink';
import { COLORS } from '../themes/index.js';

const BOOT_MSGS = [
    'kernel init',
    'loading audio pipeline',
    'scanning ffmpeg binary',
    'mounting device drivers',
    'establishing adk session',
    'warm-starting context cache',
    'loading episodic memory L3',
    'calibrating mic baseline',
    'ora ready'
];

interface BootScreenProps {
    onComplete: () => void;
}

const BootScreen: React.FC<BootScreenProps> = ({ onComplete }) => {
    const [log, setLog] = useState<{ t: string, m: string }[]>([]);
    const [index, setBi] = useState(0);

    useEffect(() => {
        if (index >= BOOT_MSGS.length) {
            const timer = setTimeout(onComplete, 800);
            return () => clearTimeout(timer);
        }

        const msg = BOOT_MSGS[index];
        const timer = setTimeout(() => {
            const timestamp = (index * 0.05).toFixed(2);
            setLog(prev => [...prev, { t: timestamp, m: msg! }]);
            setBi(i => i + 1);
        }, 60 + Math.random() * 80);

        return () => clearTimeout(timer);
    }, [index, onComplete]);

    const pct = Math.round((index / BOOT_MSGS.length) * 100);

    return (
        <Box flexDirection="column" flexGrow={1} justifyContent="center" alignItems="center" padding={4}>
            <Text bold color={COLORS.GOLD}>ORA</Text>
            
            <Box width={50} flexDirection="column" marginTop={5}>
                {log.map((entry, i) => (
                    <Box key={i} justifyContent="space-between">
                        <Box>
                            <Text color={COLORS.TEXT_GHOST}>{entry.t}s  </Text>
                            <Text color={i === BOOT_MSGS.length - 1 ? COLORS.GOLD : COLORS.TEXT_DIM}>
                                {entry.m}
                            </Text>
                        </Box>
                        <Text color={COLORS.GREEN}>ok</Text>
                    </Box>
                ))}
            </Box>

            <Box width={50} flexDirection="column" marginTop={3}>
                <Box justifyContent="space-between">
                    <Text color={COLORS.TEXT_GHOST}>INITIALIZING</Text>
                    <Text color={COLORS.TEXT_GHOST}>{pct}%</Text>
                </Box>
                <Box height={1} width="100%" backgroundColor={COLORS.BORDER_DIM}>
                    <Box width={`${pct}%`} backgroundColor={COLORS.GOLD} />
                </Box>
            </Box>

            <Box marginTop={2}>
                <Text color={index === BOOT_MSGS.length ? COLORS.GOLD : COLORS.TEXT_GHOST}>
                    {index === BOOT_MSGS.length ? 'system operational' : '—'}
                </Text>
            </Box>
        </Box>
    );
};

export default BootScreen;
