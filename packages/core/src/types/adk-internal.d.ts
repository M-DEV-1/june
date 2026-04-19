import '@google/adk';
import { z } from 'zod';

declare module '@google/adk' {
    export interface Session {
        id: string;
        appName: string;
        userId: string;
        state: Record<string, unknown>;
        events: unknown[];
        lastUpdateTime: number;
    }

    export interface InvocationContextParams {
        invocationId: string;
        agent: LlmAgent;
        session: Session;
        userContent?: unknown;
        runConfig?: unknown;
        liveRequestQueue?: LiveRequestQueue | null;
        pluginManager?: unknown;
        artifactService?: unknown;
        memoryService?: unknown;
        credentialService?: unknown;
    }

    export interface InvocationContext {
        invocationId: string;
        agent: LlmAgent;
        session: Session;
        liveRequestQueue?: LiveRequestQueue | null;
        appName: string;
        endInvocation(): void;
        invocationCostManager: any; 
        pluginManager: any;
    }

    export type Modality = 'AUDIO' | 'TEXT' | 'IMAGE' | 'VIDEO' | string;

    export interface RunConfig {
        streamingMode?: StreamingMode;
        responseModalities?: Modality[];
        maxLlmCalls?: number;
    }

    export interface Runner {
        pluginManager: any;
    }

    export type ToolInputParameters = Record<string, unknown> | undefined;

    /**
     * Converts a Zod object to a JSON schema for ADK FunctionTool.
     */
    export function zodObjectToSchema(schema: z.ZodObject<Record<string, any>> | unknown): any;
}

declare module '@ffmpeg-installer/ffmpeg' {
    const content: {
        path: string;
        version: string;
        url: string;
    };
    export default content;
}
