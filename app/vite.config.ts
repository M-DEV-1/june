import path from "node:path";
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

// @ts-expect-error process is a nodejs global
const host = process.env.TAURI_DEV_HOST;

// https://vite.dev/config/
export default defineConfig(async () => ({
  // React and Tailwind are only used by the next.html entry; the other three pages are plain TypeScript and are untouched by either plugin.
  plugins: [react(), tailwindcss()],

  // "@" is the import alias shadcn/ui generates its components against, and it points at src/ exactly as app/components.json and tsconfig.json say.
  resolve: {
    alias: { "@": path.resolve(import.meta.dirname, "./src") },
  },

  // Vite options tailored for Tauri development and only applied in `tauri dev` or `tauri build`
  //
  // 1. prevent Vite from obscuring rust errors
  clearScreen: false,
  // Three pages: index.html is the hover window, overlay.html is the click-through layer over the whole desktop that draws the daemon's rings, and next.html is the React rebuild of the main window, which Tauri now opens with the label "app".
  build: {
    rollupOptions: {
      input: { main: "index.html", overlay: "overlay.html", next: "next.html" },
    },
  },
  // 2. tauri expects a fixed port, fail if that port is not available
  server: {
    port: 1420,
    strictPort: true,
    host: host || false,
    hmr: host
      ? {
          protocol: "ws",
          host,
          port: 1421,
        }
      : undefined,
    watch: {
      // 3. tell Vite to ignore watching `src-tauri`
      ignored: ["**/src-tauri/**"],
    },
  },
}));
