import React from 'react';
import {Box} from 'ink';
import BigText from 'ink-big-text';
import Gradient from 'ink-gradient';

const Banner: React.FC = () => {
	return (
		<Box flexDirection="column" alignItems="center" marginBottom={1}>
			<Gradient name="vice">
				<BigText text="VOXPILOT" font="block" />
			</Gradient>
			<Box>
				<Gradient name="passion">
					<BigText text="   v1.0" font="tiny" />
				</Gradient>
			</Box>
		</Box>
	);
};

export default Banner;
