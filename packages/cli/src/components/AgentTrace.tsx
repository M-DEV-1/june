import React from 'react';
import { Box, Text } from 'ink';
import { stripAnsi } from '@ora/core';

export interface TraceEntry {
    agent: string;
    role?: 'orchestrator' | 'researcher' | 'librarian';
    tool?: string;
    thought?: string;
    args?: any;
    status: 'pending' | 'success' | 'error';
    durationMs?: number;
    timestamp: number;
}

interface AgentTraceProps {
    traces: TraceEntry[];
}

const AgentTrace: React.FC<AgentTraceProps> = ({ traces }) => {
    const getRoleColor = (role?: string) => {
        switch (role) {
            case 'orchestrator': return 'magenta';
            case 'researcher': return 'blue';
            case 'librarian': return 'yellow';
            default: return 'gray';
        }
    };

    return (
        <Box flexDirection="column" paddingX={1} borderStyle="round" borderColor="gray">
            <Box borderStyle="single" borderTop={false} borderLeft={false} borderRight={false} marginBottom={1}>
                <Text bold color="cyan">AGENT TRACE</Text>
            </Box>
            {traces.slice(-10).map((trace, i) => (
                <Box key={trace.timestamp + i} flexDirection="column" marginBottom={1}>
                    <Box justifyContent="space-between">
                        <Text color={getRoleColor(trace.role)}>[{trace.agent.toUpperCase()}]</Text>
                        <Text color="gray">{new Date(trace.timestamp).toLocaleTimeString([], { hour12: false, hour: '2-digit', minute: '2-digit', second: '2-digit' })}</Text>
                    </Box>
                    
                    {trace.thought && (
                        <Box paddingLeft={1} borderStyle="single" borderLeft borderRight={false} borderTop={false} borderBottom={false} borderColor="gray">
                            <Text color="gray" italic>"{stripAnsi(trace.thought.slice(0, 100))}{trace.thought.length > 100 ? '...' : ''}"</Text>
                        </Box>
                    )}

                    {trace.tool && (
                        <Box paddingLeft={2}>
                            <Text color="cyan">⚙ {trace.tool}</Text>
                            {trace.status === 'pending' ? (
                                <Text color="yellow">  (acting...)</Text>
                            ) : (
                                <Text color="green">  ✓ {trace.durationMs || '?'}ms</Text>
                            )}
                        </Box>
                    )}

                    {trace.args && trace.status === 'pending' && (
                        <Box paddingLeft={4}>
                            <Text color="gray" dimColor italic>
                                {stripAnsi(JSON.stringify(trace.args)).slice(0, 50)}...
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
