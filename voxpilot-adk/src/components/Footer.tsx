import React from 'react';
import {Box, Text} from 'ink';

const Footer: React.FC = () => {
	return (
		<Box marginTop={1} paddingX={1} justifyContent="space-around" width={70}>
			<Box>
				<Text color="gray">[1-4]</Text><Text> Mode</Text>
			</Box>
			<Box>
				<Text color="gray">[C]</Text><Text> Theme</Text>
			</Box>
			<Box>
				<Text color="gray">[+/-]</Text><Text> Gain</Text>
			</Box>
			<Box>
				<Text color="gray">[^M]</Text><Text> Mute</Text>
			</Box>
			<Box>
				<Text color="gray">[^R]</Text><Text> Reset</Text>
			</Box>
			<Box>
				<Text color="gray">[^C]</Text><Text> Quit</Text>
			</Box>
		</Box>
	);
};

export default Footer;
