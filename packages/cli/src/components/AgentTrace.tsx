import React from 'react';
import { Box, Text } from 'ink';
import { stripAnsi } from '@ora/core';

export interface TraceEntry {
    agent: string;
    role?: 'orchestrator' | 'researcher' | 'librarian';
    tool?: string;
    thought?: string;
    args?: Record<string, unknown>;
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

    const getRoleIcon = (role?: string) => {
        switch (role) {
            case 'orchestrator': return '◈';
            case 'researcher': return '🔍';
            case 'librarian': return '📚';
            default: return '●';
        }
    };

    return (
        <Box flexDirection="column" paddingX={1} borderStyle="round" borderColor="gray">
            <Box borderStyle="single" borderTop={false} borderLeft={false} borderRight={false} marginBottom={1} justifyContent="space-between">
                <Text bold color="cyan">AGENT TRACE</Text>
                <Text color="gray" dimColor>{traces.length} operations</Text>
            </Box>
            {traces.slice(-12).map((trace, i) => (
                <Box key={trace.timestamp + i} flexDirection="column" marginBottom={1}>
                    <Box justifyContent="space-between">
                        <Box>
                            <Text color={getRoleColor(trace.role)}>{getRoleIcon(trace.role)} [{trace.agent.toUpperCase()}]</Text>
                        </Box>
                        <Text color="gray" dimColor>{new Date(trace.timestamp).toLocaleTimeString([], { hour12: false, minute: '2-digit', second: '2-digit' })}</Text>
                    </Box>
                    
                    {trace.thought && (
                        <Box paddingLeft={1} borderStyle="single" borderLeft borderRight={false} borderTop={false} borderBottom={false} borderColor="gray">
                            <Text color="gray" italic dimColor>"{stripAnsi(trace.thought.slice(0, 150))}{trace.thought.length > 150 ? '...' : ''}"</Text>
                        </Box>
                    )}

                    {trace.tool && (
                        <Box paddingLeft={2}>
                            <Text color="cyan">↳ {trace.tool}</Text>
                            {trace.status === 'pending' ? (
                                <Text color="yellow"> …</Text>
                            ) : (
                                <Text color="green"> ✓ {trace.durationMs || '?'}ms</Text>
                            )}
                        </Box>
                    )}

                    {trace.args && trace.status === 'pending' && (
                        <Box paddingLeft={4}>
                            <Text color="gray" dimColor italic>
                                params: {stripAnsi(JSON.stringify(trace.args)).slice(0, 60)}...
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
