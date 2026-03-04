import React, {useState, useEffect} from 'react';
import {Box, Text} from 'ink';
import TextInput from 'ink-text-input';
import Spinner from 'ink-spinner';

interface OnboardingProps {
    onComplete: (apiKey: string) => void;
    validateApiKey?: (apiKey: string) => Promise<boolean>;
    checkDependencies?: () => Promise<{ name: string; found: boolean }[]>;
    startMicTest?: (onLevel: (level: number) => void) => () => void; // Returns a stop function
    onInstallDependency?: (name: string) => Promise<boolean>;
}

const Onboarding: React.FC<OnboardingProps> = ({onComplete, validateApiKey, checkDependencies, startMicTest, onInstallDependency}) => {
    const [step, setStep] = useState(0);
    const [apiKey, setApiKey] = useState('');
    const [navValue, setNavValue] = useState('');
    const [isValidating, setIsValidating] = useState(false);
    const [error, setError] = useState<string | null>(null);
    const [audioLevel, setAudioLevel] = useState(0);
    const [deps, setDeps] = useState<{ name: string; found: boolean }[]>([]);
    const [isCheckingDeps, setIsCheckingDeps] = useState(false);
    const [installingDep, setInstallingDep] = useState<string | null>(null);
    const [playbackActive, setPlaybackActive] = useState(false);
    const [selectedVoice, setSelectedVoice] = useState(0);

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
            refreshDeps();
        }
    }, [step]);

    const handleInstall = async (name: string) => {
        if (!onInstallDependency) return;
        setInstallingDep(name);
        try {
            const success = await onInstallDependency(name);
            if (success) {
                refreshDeps();
            } else {
                setError(`Failed to install ${name}`);
            }
        } catch (e) {
            setError(`Error installing ${name}`);
        }
        setInstallingDep(null);
    };

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
            setIsValidating(false);
            setStep(2);
        } catch (e) {
            setError('Validation failed');
            setIsValidating(false);
        }
    };

    const handleMicTestComplete = () => {
        setPlaybackActive(true);
        setTimeout(() => {
            setPlaybackActive(false);
            setStep(3);
        }, 2000);
    };

    return (
        <Box flexDirection="column" borderStyle="double" borderColor="yellow" padding={2} width={60}>
            {step === 0 && (
                <Box flexDirection="column">
                    <Text bold color="cyan">{"Welcome to VOXPILOT Commander."}</Text>
                    <Box marginTop={1}>
                        <Text>{"Your ultra-low-latency voice interface to Gemini 2.0."}</Text>
                    </Box>
                    <Box marginTop={1} flexDirection="column">
                        <Text underline>{"Dependency Check:"}</Text>
                        {isCheckingDeps ? (
                            <Text color="yellow"><Spinner type="dots" /> Checking system capabilities...</Text>
                        ) : (
                            deps.map(d => (
                                <Text key={d.name} color={d.found ? 'green' : 'red'}>
                                    {d.found ? `  [✔] ${d.name} found` : `  [✘] ${d.name} NOT FOUND`}
                                </Text>
                            ))
                        )}
                        {!isCheckingDeps && deps.every(d => !d.found) && (
                            <Box marginTop={1} paddingX={1} borderStyle="single" borderColor="red">
                                <Text color="red">{"CRITICAL: No audio capture program (sox/ffmpeg) found."}</Text>
                            </Box>
                        )}
                    </Box>
                    <Box marginTop={1}>
                        <Text color="gray">{"Press Enter to begin setup..."}</Text>
                    </Box>
                    <TextInput value={navValue} onChange={setNavValue} onSubmit={() => { setNavValue(''); setStep(1); }} />
                </Box>
            )}

            {step === 1 && (
                <Box flexDirection="column">
                    <Text bold color="cyan">{"STEP 1: IDENTITY VERIFICATION"}</Text>
                    <Box marginTop={1}>
                        <Text>{"Please enter your Google Gemini API Key:"}</Text>
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
                            <Text color="yellow">
                                <Spinner type="dots" />
                                <Text>{" Validating connection..."}</Text>
                            </Text>
                        </Box>
                    )}
                    {error && (
                        <Box marginTop={1}>
                            <Text color="red">{`ERROR: ${error}`}</Text>
                        </Box>
                    )}
                    <Box marginTop={1}>
                        <Text color="gray">{"Keys can be obtained from AI Studio."}</Text>
                    </Box>
                </Box>
            )}

            {step === 2 && (
                <Box flexDirection="column">
                    <Text bold color="cyan">{"STEP 2: BIOMETRIC CALIBRATION"}</Text>
                    <Box marginTop={1}>
                        <Text>{"Initializing default system microphone..."}</Text>
                    </Box>
                    <Box marginTop={1} height={4} borderStyle="round" borderColor="green" alignItems="center" justifyContent="center">
                        <Text color="green">
                            <Text>{"█".repeat(Math.floor(audioLevel * 20))}</Text>
                            <Text color="gray">{"░".repeat(20 - Math.floor(audioLevel * 20))}</Text>
                        </Text>
                    </Box>
                    <Box marginTop={1}>
                        {playbackActive ? (
                            <Text color="blue">
                                <Spinner type="arc" />
                                <Text>{" Replaying your voice for loopback test..."}</Text>
                            </Text>
                        ) : (
                            <Text>
                                <Text>{"Speak to test levels. Input detected: "}</Text>
                                <Text color="green">{`${(audioLevel * 100).toFixed(0)}%`}</Text>
                            </Text>
                        )}
                    </Box>
                    <Box marginTop={1}>
                        <Text color="gray">{"Press Enter to test loopback and finalize..."}</Text>
                    </Box>
                    {!playbackActive && <TextInput value={navValue} onChange={setNavValue} onSubmit={() => { setNavValue(''); handleMicTestComplete(); }} />}
                </Box>
            )}

            {step === 3 && (
                <Box flexDirection="column">
                    <Text bold color="cyan">{"STEP 3: NEURAL SYNTHETIC VOICE"}</Text>
                    <Box marginTop={1}>
                        <Text>{"Select agent personality (use up/down arrows):"}</Text>
                    </Box>
                    <Box flexDirection="column" marginTop={1}>
                        {voices.map((voice, i) => (
                            <Text key={voice} color={i === selectedVoice ? 'magenta' : 'gray'}>
                                {`  ${i === selectedVoice ? '[X]' : '[ ]'} ${voice}`}
                            </Text>
                        ))}
                    </Box>
                    <Box marginTop={2}>
                        <Text color="green" inverse>{" PRESS ENTER TO INITIALIZE SYSTEM "}</Text>
                    </Box>
                    <TextInput value={navValue} onChange={setNavValue} onSubmit={() => onComplete(apiKey)} />
                </Box>
            )}
        </Box>
    );
};

export default Onboarding;
