import React from 'react';
import { Box, Text } from 'ink';

export interface TraceEntry {
    agent: string;
    tool: string;
    args: any;
    status: 'pending' | 'success' | 'error';
    durationMs?: number;
    timestamp: number;
}

interface AgentTraceProps {
    traces: TraceEntry[];
}

const AgentTrace: React.FC<AgentTraceProps> = ({ traces }) => {
    return (
        <Box flexDirection="column" paddingX={1} borderStyle="round" borderColor="gray">
            <Box borderStyle="single" borderTop={false} borderLeft={false} borderRight={false} marginBottom={1}>
                <Text bold color="cyan">AGENT TRACE</Text>
            </Box>
            {traces.slice(-8).map((trace, i) => (
                <Box key={trace.timestamp + i} flexDirection="column" marginBottom={1}>
                    <Box justifyContent="space-between">
                        <Text color="magenta">[{trace.agent}]</Text>
                        <Text color="gray">{new Date(trace.timestamp).toLocaleTimeString([], { hour12: false, hour: '2-digit', minute: '2-digit', second: '2-digit' })}</Text>
                    </Box>
                    <Box paddingLeft={2}>
                        <Text color="blue">→ {trace.tool}</Text>
                        {trace.status === 'pending' ? (
                            <Text color="yellow">  (thinking...)</Text>
                        ) : (
                            <Text color="green">  ✓ {trace.durationMs}ms</Text>
                        )}
                    </Box>
                    {trace.args && (
                        <Box paddingLeft={4}>
                            <Text color="gray" dimColor italic>
                                {JSON.stringify(trace.args).slice(0, 60)}...
                            </Text>
                        </Box>
                    )}
                </Box>
            ))}
            {traces.length === 0 && (
                <Text color="gray" italic>Waiting for autonomous reasoning...</Text>
            )}
        </Box>
    );
};

export default AgentTrace;
