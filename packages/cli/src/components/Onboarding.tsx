import React, {useState, useEffect} from 'react';
import {Box, Text, useInput} from 'ink';
import TextInput from 'ink-text-input';
import Spinner from 'ink-spinner';

interface OnboardingProps {
    onComplete: (apiKey: string) => void;
    checkDependencies?: () => Promise<{ name: string; found: boolean }[]>;
}

const Onboarding: React.FC<OnboardingProps> = ({onComplete, checkDependencies}) => {
    const [step, setStep] = useState(0);
    const [apiKey, setApiKey] = useState(process.env.GEMINI_API_KEY || '');
    const [navValue, setNavValue] = useState('');
    const [deps, setDeps] = useState<{ name: string; found: boolean }[]>([]);
    const [isCheckingDeps, setIsCheckingDeps] = useState(false);

    useInput((input, key) => {
        if (step !== 2) return;
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
            refreshDeps();
        }
    }, [step]);

    const handleSubmitApiKey = async () => {
        if (apiKey.length < 10) {
            return;
        }
        setStep(2);
    };

    return (
        <Box flexDirection="column" borderStyle="single" borderColor="gray" paddingX={2} paddingY={1} width={70}>
            {step === 0 && (
                <Box flexDirection="column">
                    <Text bold color="cyan">ORA SYSTEM SETUP</Text>
                    <Box marginTop={1}>
                        <Text>Checking neural interface requirements...</Text>
                    </Box>
                    <Box marginTop={1} flexDirection="column">
                        <Text underline dimColor>System Dependencies:</Text>
                        {isCheckingDeps ? (
                            <Text color="yellow"> <Spinner type="dots" /> Verifying core binaries...</Text>
                        ) : (
                            deps.map(d => (
                                <Text key={d.name} color={d.found ? 'green' : 'red'}>
                                    {d.found ? `  ✔ ${d.name}` : `  ✘ ${d.name}`}
                                </Text>
                            ))
                        )}
                    </Box>
                    <Box marginTop={1}>
                        <Text color="gray">Press Enter to continue...</Text>
                    </Box>
                    <TextInput value={navValue} onChange={setNavValue} onSubmit={() => { setNavValue(''); setStep(1); }} />
                </Box>
            )}

            {step === 1 && (
                <Box flexDirection="column">
                    <Text bold color="cyan">API AUTHENTICATION</Text>
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
                    <Box marginTop={1}>
                        <Text color="gray">Key should be at least 10 characters.</Text>
                    </Box>
                </Box>
            )}

            {step === 2 && (
                <Box flexDirection="column" alignItems="center">
                    <Text bold color="cyan">READY FOR BOOT</Text>
                    <Box marginTop={1}>
                        <Text>Neural uplink established via ADK.</Text>
                    </Box>
                    <Box marginTop={2}>
                        <Text inverse color="green"> PRESS ENTER TO START SESSION </Text>
                    </Box>
                </Box>
            )}
        </Box>
    );
};

export default Onboarding;
