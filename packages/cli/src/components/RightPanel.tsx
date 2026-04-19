import React, { useState, useEffect } from 'react';
import { Box, Text } from 'ink';
import { AppState } from '../types/ui.js';
import { COLORS } from '../themes/index.js';
import Transcript from './Transcript.js';
import Waveform from './Waveform.js';

interface RightPanelProps {
    state: AppState;
}

const MemoryEventLine: React.FC<{ event: AppState['memoryEvent'] }> = ({ event }) => {
    const [opacity, setOpacity] = useState(1);

    useEffect(() => {
        if (!event) return;
        const timer = setTimeout(() => {
            const interval = setInterval(() => {
                setOpacity(o => Math.max(0, o - 0.33));
            }, 333);
            return () => clearInterval(interval);
        }, 4000);
        return () => clearTimeout(timer);
    }, [event]);

    if (!event || opacity <= 0) return null;

    return (
        <Box paddingX={2} paddingY={0} height={1}>
            <Text color="#38bdf8">⟳ </Text>
            <Text color={COLORS.TEXT_GHOST}>Memory compacted: saved ~{event.tokensSaved} tokens  </Text>
            <Text color={COLORS.GREEN}>✓</Text>
        </Box>
    );
};

const RightPanel: React.FC<RightPanelProps> = ({ state }) => {
    return (
        <Box flexDirection="column" flexGrow={1} minWidth={0}>
            {/* Main Transcript Area */}
            <Box flexGrow={1} minHeight={0}>
                <Transcript messages={state.transcript} />
            </Box>

            {/* Memory Event Notification (1 line) */}
            <MemoryEventLine event={state.memoryEvent} />

            {/* Oscilloscope Waveform Area */}
            <Box 
                borderStyle="single"
                borderBottom={false}
                borderLeft={false}
                borderRight={false}
                borderColor={COLORS.BORDER} 
                paddingX={3} 
                paddingY={1}
                flexDirection="column"
            >
                <Box justifyContent="space-between" marginBottom={1}>
                    <Text color={COLORS.TEXT_GHOST}>OSCILLOSCOPE</Text>
                    <Text color={COLORS.TEXT_GHOST}>WARM · 30 FPS</Text>
                </Box>
                <Waveform isProcessing={state.status === 'PROCESSING'} />
            </Box>
        </Box>
    );
};

export default RightPanel;
