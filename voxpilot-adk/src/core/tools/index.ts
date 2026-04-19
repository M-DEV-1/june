import { GoogleSearchTool, FunctionTool } from '@google/adk';
import { z } from 'zod';
import { promises as fs } from 'node:fs';
import path from 'node:path';
import { findingsDir } from '../config/paths.js';

export const listDirectory = new FunctionTool({
    name: 'list_directory',
    description: 'Lists files in a given directory path.',
    parameters: z.object({
        path: z.string().default('.').describe('The directory path to list.')
    }) as any,
    execute: async (args: any) => {
        const { path: p } = args;
        try {
            const target = path.resolve(process.cwd(), p || '.');
            if (!target.startsWith(process.cwd())) throw new Error("Path traversal blocked.");
            const files = await fs.readdir(target);
            return { files };
        } catch (e: unknown) {
            return { error: `Failed to list directory: ${(e as Error).message}` };
        }
    }
});

export const readFile = new FunctionTool({
    name: 'read_research_file',
    description: 'Reads the content of a specific file.',
    parameters: z.object({
        path: z.string().describe('Path to the file to read.')
    }) as any,
    execute: async (args: any) => {
        const { path: p } = args;
        try {
            const target = path.resolve(process.cwd(), p);
            if (!target.startsWith(process.cwd())) throw new Error("Path traversal blocked.");
            const content = await fs.readFile(target, 'utf-8');
            return { content: content.slice(0, 10000) };
        } catch (e: unknown) {
            return { error: `Failed to read file: ${(e as Error).message}` };
        }
    }
});

export const saveResearchNotes = new FunctionTool({
    name: 'save_research_notes',
    description: 'Saves synthesized findings or notes to the local findings knowledge base.',
    parameters: z.object({
        filename: z.string().describe('The filename for the notes (e.g., summary.md).'),
        content: z.string().describe('The synthesized research notes or summary.')
    }) as any,
    execute: async (args: any) => {
        const { filename, content } = args;
        try {
            await fs.mkdir(findingsDir, { recursive: true });
            const target = path.join(findingsDir, path.basename(filename));
            await fs.writeFile(target, content, 'utf-8');
            return { success: true, message: `Notes successfully saved to ${target}` };
        } catch (e: unknown) {
            return { error: `Failed to save notes: ${(e as Error).message}` };
        }
    }
});

export const webFetch = new FunctionTool({
    name: 'web_fetch',
    description: 'Fetches the content of a webpage and returns the text.',
    parameters: z.object({
        url: z.string().url().describe('The URL of the webpage to fetch.')
    }) as any,
    execute: async (args: any) => {
        const { url } = args;
        try {
            const response = await fetch(url);
            const html = await response.text();
            const text = html
                .replace(/<script\b[^>]*>([\s\S]*?)<\/script>/gmi, '')
                .replace(/<style\b[^>]*>([\s\S]*?)<\/style>/gmi, '')
                .replace(/<[^>]+>/g, ' ')
                .replace(/\s+/g, ' ')
                .trim();
            return { content: text.slice(0, 15000) };
        } catch (e: unknown) {
            return { error: `Failed to fetch URL: ${(e as Error).message}` };
        }
    }
});
