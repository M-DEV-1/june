import React, { useState, useEffect } from 'react';
import { Box, Text } from 'ink';
import Spinner from 'ink-spinner';
import { ExtendedOraStatus } from '../types/ui.js';
import { COLORS } from '../themes/index.js';

interface HeaderProps {
    status: ExtendedOraStatus;
}

const Header: React.FC<HeaderProps> = ({ status }) => {
    const [pulseFrame, setPulseFrame] = useState(0);

    useEffect(() => {
        if (status !== 'LISTENING') return;
        const interval = setInterval(() => {
            setPulseFrame(f => (f + 1) % 3);
        }, 800);
        return () => clearInterval(interval);
    }, [status]);

    const renderStatusIndicator = () => {
        switch (status) {
            case 'LISTENING':
                const chars = ['●', '◉', '○'];
                return <Text color={COLORS.GREEN}>{chars[pulseFrame]}</Text>;
            case 'PROCESSING':
            case 'CONNECTING':
            case 'BOOTING':
                return <Text color={COLORS.GOLD}><Spinner type="dots" /></Text>;
            case 'SPEAKING':
                return <Text color={COLORS.GOLD}>▶</Text>;
            case 'ERROR':
                return <Text color={COLORS.RED}>✖</Text>;
            default:
                return <Text color={COLORS.TEXT_GHOST}>○</Text>;
        }
    };

    const getStatusColor = () => {
        switch (status) {
            case 'LISTENING': return COLORS.GREEN;
            case 'SPEAKING': return COLORS.GOLD;
            case 'PROCESSING':
            case 'CONNECTING':
            case 'BOOTING': return COLORS.GOLD;
            case 'ERROR': return COLORS.RED;
            default: return COLORS.TEXT_GHOST;
        }
    };

    return (
        <Box 
            flexDirection="row" 
            justifyContent="space-between" 
            borderStyle="single"
            borderTop={false}
            borderLeft={false}
            borderRight={false}
            borderColor={COLORS.BORDER}
            paddingX={2}
        >
            <Box>
                <Text bold color={COLORS.GOLD}>ORA</Text>
                <Text color={COLORS.TEXT_GHOST}>  ·  </Text>
                <Box marginRight={1}>{renderStatusIndicator()}</Box>
                <Text color={getStatusColor()}>{status}</Text>
            </Box>
            <Text color={COLORS.TEXT_GHOST}>ADK-DRIVEN · GEMINI 2.0</Text>
        </Box>
    );
};

export default Header;
