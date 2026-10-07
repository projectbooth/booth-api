/// <reference types="vitest/config" />
import { fileURLToPath } from "node:url";
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Two personalities, one config (ADR 0030) — the same arrangement as the sibling modules' UI
// packages:
//   - `vite`/`vite dev`: the dev harness (index.html → src/main.tsx → src/devshell/DevShell.tsx),
//     a stand-in for booth-design's shell.
//   - `vite build`: the publishable library (@projectbooth/api-ui) from src/index.ts, with
//     react/react-dom external so booth-design's own copies are used.
export default defineConfig(({ command }) => ({
  plugins: [react()],
  build:
    command === "build"
      ? {
          lib: {
            entry: fileURLToPath(new URL("./src/index.ts", import.meta.url)),
            formats: ["es"],
            fileName: "index",
          },
          rollupOptions: {
            external: ["react", "react-dom", "react/jsx-runtime"],
          },
        }
      : undefined,
  test: {
    environment: "jsdom",
    globals: true,
    setupFiles: ["./src/setupTests.ts"],
  },
}));
