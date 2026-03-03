import React from 'react';
import {Box, Text} from 'ink';
import type { VoxPilotStatus } from '../types/index.js';
import Spinner from 'ink-spinner';

interface StatusBarProps {
    status: VoxPilotStatus;
    activeTools: string[];
	fps: number;
}

const StatusBar: React.FC<StatusBarProps> = ({status, activeTools, fps}) => {
	const getStateColor = () => {
		switch (status) {
			case 'LISTENING': return 'cyan';
			case 'PROCESSING': return 'yellow';
			case 'SPEAKING': return 'magenta';
			case 'IDLE': return 'gray';
            case 'ERROR': return 'red';
			default: return 'white';
		}
	};

	return (
		<Box borderStyle="single" borderColor="gray" paddingX={1} justifyContent="space-between" width={70}>
			<Box>
				<Box marginRight={2}>
				{status === 'CONNECTING' || status === 'PROCESSING' ? (
					<Text color="green"><Spinner type="dots" /> {status}</Text>
				) : (
					<Text bold color={getStateColor()}>{status}</Text>
				)}
				</Box>
				{activeTools.length > 0 && (
					<Box marginLeft={2}>
						<Text color="gray">[</Text>
						{activeTools.map((tool, i) => (
							<Text key={tool} color="green">
								{`${tool}${i < activeTools.length - 1 ? ', ' : ''}`}
							</Text>
						))}
						<Text color="gray">]</Text>
					</Box>
				)}
			</Box>
			<Box>
				<Text color="gray">{`${fps} FPS`}</Text>
			</Box>
		</Box>
	);
};

export default StatusBar;
