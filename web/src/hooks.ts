import { useCallback, useEffect, useRef, useState } from "react";

export type LoadState<T> = { status: "loading" } | { status: "error"; error: string; httpStatus?: number } | { status: "ready"; data: T };

/** A hand-rolled fetch state machine (loading → ready | error), as ARCHITECTURE.md §6 recommends
 *  over a data-fetching library — the same shape booth-catalog uses. Reruns when `deps` change and
 *  ignores a response that arrives after a newer request started, so a quick workspace switch can
 *  never show the previous workspace's data. `enabled: false` skips loading entirely. */
export function useLoad<T>(load: () => Promise<T>, deps: unknown[], enabled = true): { state: LoadState<T>; reload: () => void } {
  const [state, setState] = useState<LoadState<T>>({ status: "loading" });
  const latest = useRef(load);
  latest.current = load;
  const generation = useRef(0);

  const run = useCallback(
    (showSpinner: boolean) => {
      if (!enabled) return;
      const mine = ++generation.current;
      if (showSpinner) setState({ status: "loading" });
      latest.current().then(
        (data) => {
          if (mine === generation.current) setState({ status: "ready", data });
        },
        (err: unknown) => {
          if (mine !== generation.current) return;
          const httpStatus = typeof err === "object" && err !== null && "status" in err ? Number((err as { status: unknown }).status) : undefined;
          setState({ status: "error", error: err instanceof Error ? err.message : String(err), httpStatus });
        },
      );
    },
    [enabled],
  );

  useEffect(() => {
    run(true);
  }, [...deps, run]);

  const reload = useCallback(() => run(false), [run]);
  return { state, reload };
}
