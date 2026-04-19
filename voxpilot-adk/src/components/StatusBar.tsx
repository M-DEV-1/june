import React from 'react';
import {Box, Text} from 'ink';
import type { VoxPilotStatus } from '../types/index.js';
import Spinner from 'ink-spinner';

interface StatusBarProps {
    status: VoxPilotStatus;
    activeTools: string[];
	fps: number;
    latency?: number;
}

const StatusBar: React.FC<StatusBarProps> = ({status, activeTools, fps, latency}) => {
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
		<Box flexDirection="column" width="100%">
            <Box justifyContent="space-between">
                <Text bold color={getStateColor()}>● {status}</Text>
                <Text color="gray">{fps} FPS</Text>
            </Box>
            
            <Box marginTop={1}>
                <Text color="gray">Latency: </Text>
                <Text color={!latency || latency < 200 ? 'green' : 'yellow'}>{latency || 0}ms</Text>
            </Box>

            {activeTools.length > 0 && (
                <Box marginTop={1} flexDirection="column">
                    <Text color="gray" underline>Active Tools:</Text>
                    {activeTools.map((tool) => (
                        <Text key={tool} color="green">
                            › {tool.toUpperCase()}
                        </Text>
                    ))}
                </Box>
            )}
        </Box>
	);
};

export default StatusBar;
