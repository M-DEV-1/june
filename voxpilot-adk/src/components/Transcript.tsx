import React from 'react';
import {Box, Text} from 'ink';

interface Message {
	role: 'user' | 'agent' | 'system';
	text: string;
}

interface TranscriptProps {
	messages: Message[];
}

const Transcript: React.FC<TranscriptProps> = ({messages}) => {
	// Show last 12 messages for high-density view
	const displayMessages = messages.slice(-12);

	return (
		<Box flexDirection="column" paddingX={1} flexGrow={1}>
			{displayMessages.map((msg, i) => (
				<Box key={i} marginBottom={1} flexDirection="column">
                    <Box>
                        <Text bold color={msg.role === 'user' ? 'cyan' : msg.role === 'agent' ? 'magenta' : 'gray'}>
                            {msg.role === 'user' ? '■ USER' : msg.role === 'agent' ? '■ VOXPILOT' : '■ SYSTEM'}
                        </Text>
                    </Box>
					<Box paddingLeft={2}>
                        <Text dimColor={msg.role === 'system'}>{msg.text}</Text>
                    </Box>
				</Box>
			))}
			{messages.length === 0 && (
                <Box height="100%" alignItems="center" justifyContent="center">
                    <Text color="gray" italic>Connecting to neural uplink...</Text>
                </Box>
            )}
		</Box>
	);
};

export default Transcript;
