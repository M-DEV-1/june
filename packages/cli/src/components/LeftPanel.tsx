import React, { useState, useEffect } from 'react';
import { Box, Text } from 'ink';
import { AppState, TraceEntry } from '../types/ui.js';
import { COLORS } from '../themes/index.js';
import { stripAnsi } from '@ora/core';

interface LeftPanelProps {
    state: AppState;
}

const LeftPanel: React.FC<LeftPanelProps> = ({ state }) => {
    const [pendingDots, setPendingDots] = useState('.');

    useEffect(() => {
        const interval = setInterval(() => {
            setPendingDots(d => d.length >= 3 ? '.' : d + '.');
        }, 400);
        return () => clearInterval(interval);
    }, []);

    const renderMeter = (level: number, label: string, color: string) => {
        const width = 16;
        const filled = Math.round(level * width);
        const bar = '█'.repeat(filled) + '░'.repeat(width - filled);
        return (
            <Box>
                <Text color={COLORS.TEXT_GHOST}>{label.padEnd(4)}</Text>
                <Text color={color}>{bar}</Text>
                <Text color={color}> {(level * 100).toFixed(0).padStart(3)}%</Text>
            </Box>
        );
    };

    const renderStat = (key: string, value: string | number, color: string = COLORS.TEXT) => (
        <Box>
            <Text color={COLORS.TEXT_GHOST}>{key.toUpperCase().padStart(8)} </Text>
            <Text color={color}>{value}</Text>
        </Box>
    );

    const getLatencyColor = (ms: number) => {
        if (ms < 150) return COLORS.GREEN;
        if (ms < 400) return COLORS.GOLD;
        return COLORS.RED;
    };

    const getTraceColor = (agent: string) => {
        switch (agent) {
            case 'orch': return COLORS.GOLD;
            case 'rsrch': return COLORS.AGENT_RESEARCH;
            case 'shell': return COLORS.AGENT_SHELL;
            case 'code': return COLORS.AGENT_CODE;
            default: return COLORS.TEXT;
        }
    };

    const getTraceBg = (agent: string) => {
        switch (agent) {
            case 'orch': return COLORS.GOLD_SOFT;
            case 'rsrch': return '#0f1e2a';
            case 'shell': return '#1e1508';
            case 'code': return '#081e14';
            default: return COLORS.BG_RAISED;
        }
    };

    const divider = <Text color={COLORS.TEXT_GHOST}>{'─'.repeat(26)}</Text>;

    return (
        <Box flexDirection="column" width={28} paddingX={1}>
            {/* SIGNAL */}
            <Box marginTop={1} flexDirection="column">
                <Text color={COLORS.TEXT_GHOST}>SIGNAL</Text>
                {renderMeter(state.micLevel, 'MIC', COLORS.GOLD)}
                {renderMeter(state.spkLevel, 'SPK', COLORS.GOLD)}
            </Box>

            <Box marginTop={1}>{divider}</Box>

            {/* SYSTEM */}
            <Box marginTop={1} flexDirection="column">
                <Text color={COLORS.TEXT_GHOST}>SYSTEM</Text>
                {renderStat('LATENCY', `${state.latencyMs}ms`, getLatencyColor(state.latencyMs))}
                {renderStat('TOKENS', `${(state.tokenCount/1000).toFixed(1)}k / ${(state.tokenBudget/1000).toFixed(0)}k`, COLORS.GOLD)}
                {renderStat('MEMORY', state.memoryTier, COLORS.GOLD)}
                {renderStat('CACHE', state.cacheHit === true ? '✓ HIT' : state.cacheHit === false ? '✗ MISS' : '...', state.cacheHit ? COLORS.GREEN : COLORS.RED)}
                {renderStat('SESSION', new Date(state.sessionDuration * 1000).toISOString().substr(11, 8), COLORS.TEXT_GHOST)}
            </Box>

            <Box marginTop={1}>{divider}</Box>

            {/* TRACE */}
            <Box marginTop={1} flexDirection="column">
                <Text color={COLORS.TEXT_GHOST}>TRACE {state.agentTrace.length > 0 ? `(${state.agentTrace.length})` : ''}</Text>
                {state.agentTrace.length === 0 ? (
                    <Box marginY={1} justifyContent="center">
                        <Text color={COLORS.TEXT_GHOST} italic>  no operations</Text>
                    </Box>
                ) : (
                    state.agentTrace.slice(-4).map((t, i) => (
                        <Box key={t.id + i}>
                            <Box backgroundColor={getTraceBg(t.agent)} paddingX={1}>
                                <Text color={getTraceColor(t.agent)}>{`[${t.agent}]`}</Text>
                            </Box>
                            <Text color={COLORS.TEXT}> {t.tool?.slice(0, 11) || 'thought'}</Text>
                            {t.status === 'pending' ? (
                                <Text color={COLORS.GOLD}> {pendingDots}</Text>
                            ) : t.status === 'success' ? (
                                <Text color={COLORS.GREEN}> ✓</Text>
                            ) : (
                                <Text color={COLORS.RED}> ✗</Text>
                            )}
                        </Box>
                    ))
                )}
            </Box>
        </Box>
    );
};

export default LeftPanel;
