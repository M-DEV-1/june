import { LlmAgent, InMemoryRunner } from '@google/adk';
import { eventBus } from '../agent/EventBus.js';

export interface Turn {
    role: 'user' | 'agent';
    text: string;
}

export class ContextManager {
    private hotWindow: Turn[] = [];
    private episodicSummary: string = '';
    private readonly HOT_TOKEN_THRESHOLD = 6000;
    private compressionRunner: InMemoryRunner;

    constructor() {
        const compressionAgent = new LlmAgent({
            name: 'memory_compressor',
            model: 'gemini-1.5-flash', 
            instruction: `
                Compress the provided conversation turns into a structured episodic summary.
                Preserve: key decisions, facts established, open questions, and tool results.
                Format: Bullet points. Max 500 tokens.
            `
        });
        this.compressionRunner = new InMemoryRunner({
            agent: compressionAgent,
            appName: 'ora-memory-worker'
        });
    }

    async ingestTurn(turn: Turn) {
        this.hotWindow.push(turn);

        const estimatedTokens = this.estimateTokens(JSON.stringify(this.hotWindow));
        if (estimatedTokens > this.HOT_TOKEN_THRESHOLD) {
            await this.compress();
        }
    }
    private estimateTokens(text: string): number {
        return Math.ceil(text.length / 4);
    }

    private async compress() {
        const count = Math.floor(this.hotWindow.length * 0.4);
        const toCompress = this.hotWindow.splice(0, count);
        
        try {
            // Using runner.runEphemeral for one-off background task
            const result = this.compressionRunner.runEphemeral({
                userId: 'memory_worker',
                newMessage: {
                    parts: [{ text: `Previous Summary: ${this.episodicSummary}\n\nNew Turns to Integrate:\n${JSON.stringify(toCompress)}` }]
                }
            });

            let summaryText = '';
            for await (const chunk of result as any) {
                if (chunk.content?.parts) {
                    summaryText += chunk.content.parts.map((p: any) => p.text).join('');
                }
            }

            if (summaryText) {
                this.episodicSummary = summaryText;
                eventBus.emitEvent({ 
                    type: 'memory:compaction', 
                    tokensSaved: this.estimateTokens(JSON.stringify(toCompress)) 
                });
            }
        } catch (e) {
            console.error('Memory compression failed:', e);
            this.hotWindow = [...toCompress, ...this.hotWindow];
        }
    }

    getSystemPromptPrefix(): string {
        if (!this.episodicSummary) return '';
        return `[EPISODIC MEMORY]\n${this.episodicSummary}\n[END MEMORY]\n\n`;
    }
}

export const contextManager = new ContextManager();
