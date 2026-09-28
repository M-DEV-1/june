/** The IPC token header name and the dev-server escape hatch for it, shared by the hover's daemon.ts and the React window's api.ts so the two clients that talk to the same daemon never drift on either spelling. */

/** The header every daemon request carries its token in. Mirrors ipctoken.HeaderName on the Go side. */
export const TOKEN_HEADER = "X-June-Token";

/** The port the Vite dev server runs on, which is the only origin devToken will hand a token to. */
const DEV_PORT = "1420";

/** The IPC token a page may take from its own URL, which only the Vite dev server's origin may do. Input: the page's port and query string. Output: the value of ?token=, or undefined on any other origin or when there is none. A packaged app is served from tauri://localhost with no port, so this is never a way into the real window; the daemon already allows CORS for the dev origin, so a browser tab opened with the token can read live data while a window is being worked on. */
export function devToken(loc: { port: string; search: string }): string | undefined {
  if (loc.port !== DEV_PORT) return undefined;
  return new URLSearchParams(loc.search).get("token") ?? undefined;
}
