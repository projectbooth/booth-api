import { useCallback, useEffect, useRef, useState } from "react";
import { ApiApp } from "../ApiApp";
import type { WorkspaceRole } from "../types";

// Stand-in for booth-design's shell — local-dev scaffolding only, never shipped (same idea as
// the sibling modules' harnesses). Exercises the props contract (ADR 0031/0033) against a locally running
// backend: paste a real token from your OIDC provider (the backend verifies it, ADR 0041).
export function DevShell() {
  const [theme, setTheme] = useState<"light" | "dark">("light");
  const [workspace, setWorkspace] = useState("acme");
  const [role, setRole] = useState<WorkspaceRole>("owner");
  const [token, setToken] = useState("");
  const tokenRef = useRef(token);
  useEffect(() => {
    tokenRef.current = token;
  }, [token]);
  const getAccessToken = useCallback(() => tokenRef.current || null, []);
  useEffect(() => {
    document.documentElement.dataset.theme = theme;
  }, [theme]);

  const input = "ml-1.5 rounded-md border border-slate-300 px-2 py-1 text-sm dark:border-slate-700 dark:bg-slate-800 dark:text-slate-100";
  return (
    <div className="min-h-screen bg-slate-50 dark:bg-slate-950">
      <header className="flex flex-wrap items-center gap-3 border-b border-slate-200 bg-white px-6 py-3 text-xs text-slate-500 dark:border-slate-800 dark:bg-slate-900 dark:text-slate-400">
        <span className="uppercase tracking-wide">Dev harness — not the real shell</span>
        <label>
          Workspace
          <input className={`${input} w-28`} value={workspace} onChange={(e) => setWorkspace(e.target.value)} />
        </label>
        <label>
          Role
          <select className={input} value={role} onChange={(e) => setRole(e.target.value as WorkspaceRole)}>
            <option>owner</option>
            <option>editor</option>
            <option>viewer</option>
          </select>
        </label>
        <label>
          Token
          <input className={`${input} w-64`} value={token} onChange={(e) => setToken(e.target.value)} placeholder="paste a bearer token" />
        </label>
        <button type="button" className={input} onClick={() => setTheme(theme === "light" ? "dark" : "light")}>
          {theme === "light" ? "Dark" : "Light"}
        </button>
      </header>
      {/* The shell owns outer padding (ADR 0072); the harness provides it the same way. */}
      <main className="p-6">
        <ApiApp workspace={workspace} role={role} theme={theme} getAccessToken={getAccessToken} />
      </main>
    </div>
  );
}
