import { FunctionTool, zodObjectToSchema, type ToolInputParameters } from '@google/adk';
import { z } from 'zod';
import { promises as fs } from 'node:fs';
import path from 'node:path';
import { findingsDir, workspaceDir } from '../config/paths.js';

interface ListDirectoryArgs {
    path?: string;
}

export const listDirectory = new FunctionTool({
    name: 'list_directory',
    description: 'Lists files in a given directory path within the workspace.',
    parameters: zodObjectToSchema(z.object({
        path: z.string().optional().default('.').describe('The directory path to list. Must be relative to workspace.')
    })) as any,
    execute: async (args: unknown) => {
        const { path: p } = (args || {}) as ListDirectoryArgs;
        try {
            // Strict sanitization: remove leading slashes and drive letters
            const sanitizedPath = (p || '.').replace(/^([a-zA-Z]:)?[\\/]+/, '');
            const target = path.resolve(workspaceDir, sanitizedPath);
            
            if (!target.startsWith(workspaceDir)) {
                throw new Error("Sandbox violation: Access outside workspace is blocked.");
            }
            
            const files = await fs.readdir(target);
            return { files };
        } catch (error: unknown) {
            const message = error instanceof Error ? error.message : String(error);
            return { error: `Failed to list directory: ${message}` };
        }
    }
});

interface ReadFileArgs {
    path: string;
}

export const readFile = new FunctionTool({
    name: 'read_research_file',
    description: 'Reads the content of a specific file within the workspace.',
    parameters: zodObjectToSchema(z.object({
        path: z.string().describe('Path to the file to read. Must be relative to workspace.')
    })) as any,
    execute: async (args: unknown) => {
        const { path: p } = (args || {}) as ReadFileArgs;
        try {
            const sanitizedPath = (p || '').replace(/^([a-zA-Z]:)?[\\/]+/, '');
            const target = path.resolve(workspaceDir, sanitizedPath);
            
            if (!target.startsWith(workspaceDir)) {
                throw new Error("Sandbox violation: Access outside workspace is blocked.");
            }

            const stats = await fs.stat(target);
            if (stats.size > 1024 * 1024) { // 1MB limit for production safety
                throw new Error("File too large. Only files under 1MB can be read.");
            }

            const content = await fs.readFile(target, 'utf-8');
            return { content };
        } catch (error: unknown) {
            const message = error instanceof Error ? error.message : String(error);
            return { error: `Failed to read file: ${message}` };
        }
    }
});

interface SaveNotesArgs {
    filename: string;
    content: string;
}

export const saveResearchNotes = new FunctionTool({
    name: 'save_research_notes',
    description: 'Saves synthesized findings or notes to the local findings knowledge base.',
    parameters: zodObjectToSchema(z.object({
        filename: z.string().regex(/^[a-zA-Z0-9._-]+$/, "Invalid filename characters").describe('The filename for the notes (e.g., summary.md).'),
        content: z.string().min(10, "Content too short").describe('The synthesized research notes or summary.')
    })) as any,
    execute: async (args: unknown) => {
        const { filename, content } = (args || {}) as SaveNotesArgs;
        try {
            await fs.mkdir(findingsDir, { recursive: true });
            const safeFilename = path.basename(filename);
            const target = path.join(findingsDir, safeFilename);
            await fs.writeFile(target, content, 'utf-8');
            return { success: true, message: `Notes successfully saved to ${target}` };
        } catch (error: unknown) {
            const message = error instanceof Error ? error.message : String(error);
            return { error: `Failed to save notes: ${message}` };
        }
    }
});

interface WebFetchArgs {
    url: string;
}

export const webFetch = new FunctionTool({
    name: 'web_fetch',
    description: 'Fetches the content of a webpage and returns the text. Only http/https supported.',
    parameters: zodObjectToSchema(z.object({
        url: z.string().url().describe('The URL of the webpage to fetch.')
    })) as any,
    execute: async (args: unknown) => {
        const { url } = (args || {}) as WebFetchArgs;
        try {
            const parsedUrl = new URL(url);
            
            if (parsedUrl.protocol !== 'http:' && parsedUrl.protocol !== 'https:') {
                throw new Error("Invalid protocol. Only http and https are supported.");
            }

            const hostname = parsedUrl.hostname.toLowerCase();
            const isPrivate = (host: string) => {
                const privatePatterns = [
                    /^localhost$/, /^127\./, /^10\./, /^172\.(1[6-9]|2[0-9]|3[0-1])\./,
                    /^192\.168\./, /^169\.254\./, /^0\./, /^::1$/, /^fe80:/, /^fc00:/, /^fd00:/
                ];
                return privatePatterns.some(p => p.test(host)) || host.endsWith('.local') || host.endsWith('.internal');
            };

            if (isPrivate(hostname)) {
                throw new Error("Access to internal/private hosts is blocked.");
            }

            const response = await fetch(url, {
                headers: { 'User-Agent': 'Ora/2.0 (Research Conductor)' },
                signal: AbortSignal.timeout(10000) // 10s production timeout
            });

            if (!response.ok) {
                throw new Error(`HTTP Error: ${response.status} ${response.statusText}`);
            }

            const html = await response.text();
            const text = html
                .replace(/<script\b[^>]*>([\s\S]*?)<\/script>/gmi, '')
                .replace(/<style\b[^>]*>([\s\S]*?)<\/style>/gmi, '')
                .replace(/<[^>]+>/g, ' ')
                .replace(/\s+/g, ' ')
                .trim();
            return { content: text.slice(0, 15000) };
        } catch (error: unknown) {
            const message = error instanceof Error ? error.message : String(error);
            return { error: `Failed to fetch URL: ${message}` };
        }
    }
});
