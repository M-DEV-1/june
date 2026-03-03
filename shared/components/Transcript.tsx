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
	// Only show last 5 messages to keep it clean
	const displayMessages = messages.slice(-5);

	return (
		<Box flexDirection="column" marginTop={1} paddingX={1} width={70} height={10} borderStyle="round" borderColor="blue">
			{displayMessages.map((msg, i) => (
				<Box key={i} marginBottom={msg.role === 'agent' ? 1 : 0}>
					<Text bold color={msg.role === 'user' ? 'cyan' : msg.role === 'agent' ? 'magenta' : 'gray'}>
						{msg.role.toUpperCase()}:{' '}
					</Text>
					<Text>{msg.text}</Text>
				</Box>
			))}
			{messages.length === 0 && <Text color="gray">Conversation transcript will appear here...</Text>}
		</Box>
	);
};

export default Transcript;
