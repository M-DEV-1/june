import React from 'react';
import { Box, Text } from 'ink';
import { COLORS } from '../themes/index.js';

interface FooterProps {
    fps: number;
}

const Footer: React.FC<FooterProps> = ({ fps }) => {
    return (
        <Box 
            borderStyle="single"
            borderBottom={false}
            borderLeft={false}
            borderRight={false}
            borderColor={COLORS.BORDER} 
            paddingX={2} 
            justifyContent="space-between"
        >
            <Box>
                <Text color={COLORS.GOLD}>[1-4]</Text><Text color={COLORS.TEXT_GHOST}> viz  </Text>
                <Text color={COLORS.GOLD}>[C]</Text><Text color={COLORS.TEXT_GHOST}> palette  </Text>
                <Text color={COLORS.GOLD}>[^M]</Text><Text color={COLORS.TEXT_GHOST}> mute  </Text>
                <Text color={COLORS.GOLD}>[^R]</Text><Text color={COLORS.TEXT_GHOST}> reset  </Text>
                <Text color={COLORS.GOLD}>[^D]</Text><Text color={COLORS.TEXT_GHOST}> dump  </Text>
                <Text color={COLORS.GOLD}>[^C]</Text><Text color={COLORS.TEXT_GHOST}> quit</Text>
            </Box>
            <Text color={COLORS.TEXT_GHOST}>{fps} FPS</Text>
        </Box>
    );
};

export default Footer;
