import { promises as fs } from 'node:fs';
import path from 'node:path';
import { type FunctionDeclaration, Type } from '@google/genai';

export const declarations: FunctionDeclaration[] = [
    {
        name: 'list_directory',
        description: 'Lists files in a given directory path.',
        parameters: {
            type: Type.OBJECT,
            properties: {
                path: {
                    type: Type.STRING,
                    description: 'The directory path to list.'
                }
            },
            required: []
        }
    },
    {
        name: 'read_research_file',
        description: 'Reads the content of a specific file.',
        parameters: {
            type: Type.OBJECT,
            properties: {
                path: {
                    type: Type.STRING,
                    description: 'Path to the file to read.'
                }
            },
            required: ['path']
        }
    },
    {
        name: 'web_fetch',
        description: 'Fetches the content of a webpage and returns the text.',
        parameters: {
            type: Type.OBJECT,
            properties: {
                url: {
                    type: Type.STRING,
                    description: 'The URL of the webpage to fetch.'
                }
            },
            required: ['url']
        }
    },
    {
        name: 'save_research_notes',
        description: 'Saves synthesized findings or notes to a local file.',
        parameters: {
            type: Type.OBJECT,
            properties: {
                path: {
                    type: Type.STRING,
                    description: 'File path where notes should be saved.'
                },
                content: {
                    type: Type.STRING,
                    description: 'The synthesized research notes or summary.'
                }
            },
            required: ['path', 'content']
        }
    }
];

export const tools = {
    list_directory: async ({ path: p = '.' }: { path?: string }) => {
        try {
            const target = path.resolve(process.cwd(), p);
            if (!target.startsWith(process.cwd())) throw new Error("Path traversal blocked.");
            const files = await fs.readdir(target);
            return { files };
        } catch (e: unknown) {
            return { error: `Failed to list directory: ${(e as Error).message}` };
        }
    },
    read_research_file: async ({ path: p }: { path: string }) => {
        try {
            const target = path.resolve(process.cwd(), p);
            if (!target.startsWith(process.cwd())) throw new Error("Path traversal blocked.");
            const content = await fs.readFile(target, 'utf-8');
            return { content: content.slice(0, 10000) };
        } catch (e: unknown) {
            return { error: `Failed to read file: ${(e as Error).message}` };
        }
    },
    web_fetch: async ({ url }: { url: string }) => {
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
    },
    save_research_notes: async ({ path: p, content }: { path: string, content: string }) => {
        try {
            const target = path.resolve(process.cwd(), p);
            if (!target.startsWith(process.cwd())) throw new Error("Path traversal blocked.");
            await fs.writeFile(target, content, 'utf-8');
            return { success: true, message: `Notes successfully saved to ${target}` };
        } catch (e: unknown) {
            return { error: `Failed to save notes: ${(e as Error).message}` };
        }
    }
};
