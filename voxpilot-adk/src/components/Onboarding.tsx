import React, {useState, useEffect} from 'react';
import {Box, Text, useInput} from 'ink';
import TextInput from 'ink-text-input';
import Spinner from 'ink-spinner';
import fs from 'node:fs';
import path from 'node:path';

interface OnboardingProps {
    onComplete: (apiKey: string) => void;
    validateApiKey?: (apiKey: string) => Promise<boolean>;
    checkDependencies?: () => Promise<{ name: string; found: boolean }[]>;
    startMicTest?: (onLevel: (level: number) => void) => () => void;
}

const Onboarding: React.FC<OnboardingProps> = ({onComplete, validateApiKey, checkDependencies, startMicTest}) => {
    const [step, setStep] = useState(0);
    const [apiKey, setApiKey] = useState(process.env.GEMINI_API_KEY || '');
    const [navValue, setNavValue] = useState('');
    const [isValidating, setIsValidating] = useState(false);
    const [error, setError] = useState<string | null>(null);
    const [audioLevel, setAudioLevel] = useState(0);
    const [deps, setDeps] = useState<{ name: string; found: boolean }[]>([]);
    const [isCheckingDeps, setIsCheckingDeps] = useState(false);
    const [selectedVoice, setSelectedVoice] = useState(0);

    const voices = ['Kore (Default)', 'Puck (Energetic)', 'Charon (Analytical)'];

    useInput((input, key) => {
        if (step !== 3) return;
        if (key.upArrow) {
            setSelectedVoice(v => (v > 0 ? v - 1 : voices.length - 1));
        }
        if (key.downArrow) {
            setSelectedVoice(v => (v < voices.length - 1 ? v + 1 : 0));
        }
        if (key.return) {
            onComplete(apiKey);
        }
    });

    const refreshDeps = () => {
        setIsCheckingDeps(true);
        if (checkDependencies) {
            checkDependencies().then(d => {
                setDeps(d);
                setIsCheckingDeps(false);
            });
        } else {
            setIsCheckingDeps(false);
        }
    }

    useEffect(() => {
        if (step === 0) {
            const setup = async () => {
                setIsCheckingDeps(true);
                try {
                    const { setupFfmpeg } = await import('../utils/audio.js');
                    await setupFfmpeg();
                } catch (e) {}
                refreshDeps();
            };
            setup();
        }
    }, [step]);

    useEffect(() => {
        if (step !== 2 || !startMicTest) return;
        const stop = startMicTest((level) => {
            setAudioLevel(level);
        });
        return () => stop();
    }, [step]);

    const handleSubmitApiKey = async () => {
        if (apiKey.length < 10) {
            setError('Key too short');
            return;
        }
        setError(null);
        setIsValidating(true);
        
        try {
            if (validateApiKey) {
                const isValid = await validateApiKey(apiKey);
                if (!isValid) {
                    setError('Invalid API Key');
                    setIsValidating(false);
                    return;
                }
            }
            
            try {
                const envPath = path.resolve(process.cwd(), '.env');
                let envContent = '';
                if (fs.existsSync(envPath)) {
                    envContent = fs.readFileSync(envPath, 'utf8');
                }
                if (!envContent.includes('GEMINI_API_KEY=')) {
                    fs.appendFileSync(envPath, `\nGEMINI_API_KEY=${apiKey}\n`);
                }
            } catch(e) {}

            setIsValidating(false);
            setStep(2);
        } catch (e) {
            setError('Validation failed');
            setIsValidating(false);
        }
    };

    useEffect(() => {
        if (step === 1 && apiKey.length > 10) {
            handleSubmitApiKey();
        }
    }, [step]);

    return (
        <Box flexDirection="column" borderStyle="single" borderColor="gray" paddingX={2} paddingY={1} width={70}>
            {step === 0 && (
                <Box flexDirection="column">
                    <Text bold color="cyan">VOXPILOT COMMANDER SETUP</Text>
                    <Box marginTop={1}>
                        <Text>Initializing autonomous neural interface...</Text>
                    </Box>
                    <Box marginTop={1} flexDirection="column">
                        <Text underline dimColor>Dependencies:</Text>
                        {isCheckingDeps ? (
                            <Text color="yellow"> <Spinner type="dots" /> Accessing satellite uplink...</Text>
                        ) : (
                            deps.map(d => (
                                <Text key={d.name} color={d.found ? 'green' : 'red'}>
                                    {d.found ? `  ✔ ${d.name}` : `  ✘ ${d.name}`}
                                </Text>
                            ))
                        )}
                    </Box>
                    <Box marginTop={1}>
                        <Text color="gray">Press Enter to initiate verification...</Text>
                    </Box>
                    <TextInput value={navValue} onChange={setNavValue} onSubmit={() => { setNavValue(''); setStep(1); }} />
                </Box>
            )}

            {step === 1 && (
                <Box flexDirection="column">
                    <Text bold color="cyan">IDENTITY VERIFICATION</Text>
                    <Box marginTop={1}>
                        <Text>Enter Google Gemini API Key:</Text>
                    </Box>
                    <Box marginTop={1} paddingX={1} borderStyle="single" borderColor="gray">
                        <TextInput
                            value={apiKey}
                            onChange={setApiKey}
                            onSubmit={handleSubmitApiKey}
                            mask="*"
                        />
                    </Box>
                    {isValidating && (
                        <Box marginTop={1}>
                            <Text color="yellow"> <Spinner type="dots" /> Validating credentials...</Text>
                        </Box>
                    )}
                    {error && <Box marginTop={1}><Text color="red">ERROR: {error}</Text></Box>}
                </Box>
            )}

            {step === 2 && (
                <Box flexDirection="column">
                    <Text bold color="cyan">BIOMETRIC CALIBRATION</Text>
                    <Box marginTop={1}>
                        <Text>Microphone Input Level:</Text>
                    </Box>
                    <Box marginTop={1} paddingX={1} borderStyle="single" borderColor="green">
                        <Text color="green">
                            {'█'.repeat(Math.floor(audioLevel * 30))}
                            <Text color="gray">{'░'.repeat(30 - Math.floor(audioLevel * 30))}</Text>
                        </Text>
                    </Box>
                    <Box marginTop={1}>
                        <Text color="gray">Detected: {(audioLevel * 100).toFixed(0)}%</Text>
                    </Box>
                    <Box marginTop={1}>
                        <Text color="gray">Press Enter to finalize...</Text>
                    </Box>
                    <TextInput value={navValue} onChange={setNavValue} onSubmit={() => { setNavValue(''); setStep(3); }} />
                </Box>
            )}

            {step === 3 && (
                <Box flexDirection="column">
                    <Text bold color="cyan">SYNTHETIC PERSONALITY</Text>
                    <Box marginTop={1} flexDirection="column">
                        {voices.map((voice, i) => (
                            <Text key={voice} color={i === selectedVoice ? 'magenta' : 'gray'}>
                                {i === selectedVoice ? '▶ ' : '  '} {voice}
                            </Text>
                        ))}
                    </Box>
                    <Box marginTop={1}>
                        <Text inverse color="green"> PRESS ENTER TO BOOT SYSTEM </Text>
                    </Box>
                </Box>
            )}
        </Box>
    );
};

export default Onboarding;
