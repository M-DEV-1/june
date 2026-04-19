import { LlmAgent, InMemoryRunner } from '@google/adk';

interface AdkChunk {
    content?: {
        parts?: Array<{
            text?: string;
        }>;
    };
}

export class ToolResultCompressor {
    private runner: InMemoryRunner;

    constructor() {
        const agent = new LlmAgent({
            name: 'tool_compressor',
            model: 'gemini-1.5-flash',
            instruction: `
                You are a data compression specialist.
                Take the raw tool output and compress it into a high-density structured summary.
                Focus only on the information relevant to the user's implicit goal.
                Max 300 tokens.
            `
        });
        this.runner = new InMemoryRunner({
            agent,
            appName: 'ora-tool-worker'
        });
    }

    async compress(toolName: string, rawResult: unknown, userGoal: string): Promise<string> {
        const resultString = JSON.stringify(rawResult);
        if (resultString.length < 1000) return resultString;

        try {
            const result = this.runner.runEphemeral({
                userId: 'tool_worker',
                newMessage: {
                    parts: [{ text: `Tool: ${toolName}\nUser Goal: ${userGoal}\nRaw Result: ${resultString.slice(0, 30000)}` }]
                }
            });

            let summary = '';
            for await (const chunk of result as AsyncIterable<AdkChunk>) {
                if (chunk.content?.parts) {
                    summary += chunk.content.parts.filter(p => p.text).map(p => p.text).join('');
                }
            }
            return summary || resultString.slice(0, 500); // Fallback to truncated
        } catch (error: unknown) {
            console.error('Tool result compression failed:', error);
            return resultString.slice(0, 500);
        }
    }
}

export const toolResultCompressor = new ToolResultCompressor();
