import type { WorkspaceRole } from "./types";

/**
 * Props contract pinned by ADR 0031 (workspace/role/theme) and ADR 0033 (getAccessToken): plain
 * React props, nothing imported from booth-design.
 */
export interface ApiAppProps {
  /** Active workspace slug (ADR 0025). */
  workspace: string;
  /** Caller's role, for deciding what to offer only; the backend derives it from the token (ADR 0041). */
  role: WorkspaceRole;
  theme: "dark" | "light";
  /** booth-design's current bearer token, or null. Called fresh before every request (ADR 0033). */
  getAccessToken: () => string | null;
}

/**
 * The native view booth-design mounts for booth-api. Scaffold stage: it says what the module will
 * do and that it isn't available yet, and makes no requests — the management API it will call
 * doesn't exist yet. No outer padding: the shell owns it (ADR 0072).
 */
export function ApiApp({ theme }: ApiAppProps) {
  return (
    <div data-theme={theme} className="flex flex-col gap-4 text-slate-900 dark:text-slate-100">
      <header>
        <h1 className="text-xl font-semibold">API</h1>
        <p className="mt-1 text-sm text-slate-500 dark:text-slate-400">
          Read-only REST and GraphQL APIs over catalog datasets, reached with API keys issued here.
        </p>
      </header>
      <div className="rounded-md border border-slate-200 bg-white px-4 py-3 text-sm text-slate-600 dark:border-slate-800 dark:bg-slate-900 dark:text-slate-300">
        Not available yet: generating APIs and issuing keys are still being built.
      </div>
    </div>
  );
}
