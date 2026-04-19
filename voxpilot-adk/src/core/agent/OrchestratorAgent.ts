import { LlmAgent, GoogleSearchTool, AgentTool } from '@google/adk';
import { webFetch, listDirectory, readFile, saveResearchNotes } from '../tools/index.js';

// Research Subagent - Specialized in search and analysis
export const researchAgent = new LlmAgent({
    name: "researcher",
    model: "gemini-1.5-flash",
    description: "Specialized in deep search, web fetching, and synthesizing findings.",
    instruction: `
        You are an expert researcher. Use tools to find information, read it deeply, and synthesize.
        When called, perform the requested research and return a structured summary.
    `,
    tools: [
        new GoogleSearchTool(),
        webFetch
    ]
});

// File Subagent - Specialized in local filesystem operations
export const fileAgent = new LlmAgent({
    name: "librarian",
    model: "gemini-1.5-flash",
    description: "Specialized in local filesystem operations and note-taking.",
    instruction: `
        You are a librarian for the local filesystem. 
        You can list directories, read files, and save research notes.
    `,
    tools: [
        listDirectory,
        readFile,
        saveResearchNotes
    ]
});

// Root Orchestrator - Delegates to subagents
export const orchestratorAgent = new LlmAgent({
    name: "voxpilot",
    model: "gemini-2.0-flash-exp", // The primary model
    description: "The primary orchestrator of the VOXPILOT system.",
    instruction: `
        You are VOXPILOT, an autonomous research conductor.
        Your goal is to fulfill user requests by delegating to specialized agents.
        
        Style: Professional, high-signal, concise.
        
        ORCHESTRATION STRATEGY:
        1. ANALYZE: Understand the user's intent.
        2. DELEGATE: Use the researcher for web search/fetching or the librarian for filesystem tasks.
        3. NARRATE: Briefly inform the user what you are doing via voice turns.
        4. SYNTHESIZE: Combine subagent results into a final answer.
        
        Voice-first UX: Avoid long lists. Speak naturally but technically.
    `,
    tools: [
        new AgentTool(researchAgent),
        new AgentTool(fileAgent)
    ]
});
