import React, { useState, useEffect } from 'react';
import { Box, Text } from 'ink';
import { AppMessage } from '@ora/core';

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

	// Show last 12 messages for high-density view
	const displayMessages = messages.slice(-12);

	return (
		<Box flexDirection="column" paddingX={1} flexGrow={1} minHeight={10}>
			{displayMessages.map((msg, i) => {
                const isLast = i === displayMessages.length - 1;
                const showCursor = isLast && msg.role === 'agent' && msg.partial;

                return (
                    <Box key={i} marginBottom={1} flexDirection="column">
                        <Box>
                            <Text bold color={msg.role === 'user' ? 'cyan' : msg.role === 'agent' ? 'magenta' : 'gray'}>
                                {msg.role === 'user' ? 'YOU' : msg.role === 'agent' ? 'ORA' : 'SYS'}
                            </Text>
                            <Text color="gray"> ┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄</Text>
                        </Box>
                        <Box paddingLeft={1}>
                            <Text dimColor={msg.role === 'system'}>
                                {msg.text}
                                {showCursor && (
                                    <Text color="magenta">{cursorVisible ? ' ▌' : '  '}</Text>
                                )}
                            </Text>
                        </Box>
                    </Box>
                );
            })}
			{messages.length === 0 && (
                <Box height="100%" alignItems="center" justifyContent="center">
                    <Text color="gray" dimColor italic>
                        Waiting for user input or agent thought...
                    </Text>
                </Box>
            )}
		</Box>
	);
};

export default Transcript;
