// Dead-code check for the window app, run in CI as `npx knip`. The three pages are listed by hand because Vite's config is an async function knip cannot read, and CSS @import lines are turned into imports so the stylesheet dependencies count as used.
export default {
  entry: ["src/overlay/main.ts", "src/next/main.tsx", "src/styles.css", "src/overlay/overlay.css"],
  project: ["src/**/*.{ts,tsx,css}"],
  compilers: {
    css: (text: string) => [...text.matchAll(/(?<=@)import[^;]+/g)].join("\n"),
  },
  ignoreExportsUsedInFile: true,
};
