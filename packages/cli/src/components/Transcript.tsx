import React, { useState, useEffect } from 'react';
import { Box, Text } from 'ink';
import { AppMessage, stripAnsi } from '@ora/core';
import { COLORS } from '../themes/index.js';

interface TranscriptProps {
	messages: AppMessage[];
}

const Transcript: React.FC<TranscriptProps> = ({ messages }) => {
    const [cursorVisible, setCursorVisible] = useState(true);

    useEffect(() => {
        const interval = setInterval(() => {
            setCursorVisible(v => !v);
        }, 500);
        return () => clearInterval(interval);
    }, []);

	// Show last 4 exchanges
	const displayMessages = messages.slice(-8);

	return (
		<Box flexDirection="column" paddingX={3} paddingY={1} flexGrow={1} justifyContent="flex-end">
			{displayMessages.map((msg, i) => {
                const isLast = i === displayMessages.length - 1;
                const showCursor = isLast && msg.role === 'agent' && msg.partial;

                if (msg.role === 'system') {
                    return (
                        <Box key={i} marginBottom={1}>
                            <Text color={COLORS.GOLD_DIM}>⚡ </Text>
                            <Text color={COLORS.TEXT_GHOST}>{stripAnsi(msg.text)}</Text>
                        </Box>
                    );
                }

                if (msg.role === 'user') {
                    return (
                        <Box key={i} flexDirection="column" marginBottom={1}>
                            <Box>
                                <Text color={COLORS.GOLD}>YOU</Text>
                                <Text color={COLORS.TEXT_GHOST}>  ·  10:42</Text>
                            </Box>
                            <Box paddingLeft={2}>
                                <Text color={COLORS.TEXT_DIM}>{stripAnsi(msg.text)}</Text>
                            </Box>
                        </Box>
                    );
                }

                return (
                    <Box key={i} flexDirection="column" marginBottom={1}>
                        <Box>
                            <Text color="#8b5cf6">ORA</Text>
                            <Text color={COLORS.TEXT_GHOST}>  ·  10:42</Text>
                        </Box>
                        <Box borderLeft borderStyle="single" borderColor="#1e1530" paddingLeft={2}>
                            <Text color={COLORS.TEXT}>
                                {stripAnsi(msg.text)}
                                {showCursor && (
                                    <Text color={COLORS.GOLD}>{cursorVisible ? '▌' : ' '}</Text>
                                )}
                            </Text>
                        </Box>
                    </Box>
                );
            })}
			{messages.length === 0 && (
                <Box height="100%" alignItems="center" justifyContent="center">
                    <Text color={COLORS.TEXT_GHOST} italic>
                        waiting for first exchange...
                    </Text>
                </Box>
            )}
		</Box>
	);
};

export default Transcript;
