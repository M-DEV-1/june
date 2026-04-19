import React from 'react';
import {Box, Text} from 'ink';
import Gradient from 'ink-gradient';

const Banner: React.FC = () => {
	return (
		<Box alignItems="center">
			<Gradient name="vice">
				<Text bold italic underline>VOXPILOT COMMANDER</Text>
			</Gradient>
			<Box marginLeft={2}>
				<Gradient name="passion">
					<Text dimColor>v1.5 (Production)</Text>
				</Gradient>
			</Box>
		</Box>
	);
};

export default Banner;
