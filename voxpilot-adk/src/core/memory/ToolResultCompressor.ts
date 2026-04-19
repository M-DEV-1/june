import { LlmAgent } from '@google/adk';

export class ToolResultCompressor {
    private agent: LlmAgent;

    constructor() {
        this.agent = new LlmAgent({
            name: 'tool_compressor',
            model: 'gemini-1.5-flash',
            instruction: `
                You are a data compression specialist.
                Take the raw tool output and compress it into a high-density structured summary.
                Focus only on the information relevant to the user's implicit goal.
                Max 300 tokens.
            `
        });
    }

    async compress(toolName: string, rawResult: any, userGoal: string): Promise<string> {
        const resultString = JSON.stringify(rawResult);
        if (resultString.length < 1000) return resultString;

        const result = await this.agent.runAsync({
            newMessage: {
                parts: [{ text: `Tool: ${toolName}\nUser Goal: ${userGoal}\nRaw Result: ${resultString.slice(0, 30000)}` }]
            }
        });

        let summary = '';
        for await (const chunk of result as any) {
            if (chunk.content?.parts) {
                summary += chunk.content.parts.map((p: any) => p.text).join('');
            }
        }
        return summary;
    }
}

export const toolResultCompressor = new ToolResultCompressor();
