/**
 * Production-grade sanitization utilities for terminal output.
 */

/**
 * Strips ANSI escape sequences from a string to prevent terminal injection.
 * @param text The raw string potentially containing escape sequences.
 */
export function stripAnsi(text: string): string {
    const ansiRegex = /[\u001b\u009b][[()#;?]*(?:[0-9]{1,4}(?:;[0-9]{0,4})*)?[0-9A-ORZcf-nqry=><]/g;
    return text.replace(ansiRegex, '');
}
