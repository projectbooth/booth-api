import { useMemo, useState, type FormEvent, type ReactNode } from "react";
import {
  ApiError,
  deleteApi,
  disableWrites,
  generateApi,
  createGrant,
  getWrites,
  getOpenAPI,
  getSchema,
  issueKey,
  listApis,
  listGrants,
  listKeys,
  listPostgresDatasets,
  regenerateApi,
  revokeGrant,
  revokeKey,
  saveWrites,
  type ApiContext,
  type GetAccessToken,
} from "./api/client";
import { useLoad, type LoadState } from "./hooks";
import type { ApiDefinition, ApiKey, IssuedKey, Mismatch, WorkspaceRole, WriteGrant, WriteOperation, WriteSettings, WritesView } from "./types";

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
  getAccessToken: GetAccessToken;
}

/**
 * The native view booth-design mounts for booth-api (ADR 0030): the workspace's generated APIs and,
 * for editors and owners, generating new ones and managing API keys (ADR 0100). No outer padding:
 * the shell owns it (ADR 0072). Class names are complete static strings so booth-design's single
 * Tailwind build can find them (ADR 0097).
 */
export function ApiApp({ workspace, role, theme, getAccessToken }: ApiAppProps) {
  const api = useMemo<ApiContext>(() => ({ workspace, getAccessToken }), [workspace, getAccessToken]);
  const canWrite = role === "owner" || role === "editor";
  const isOwner = role === "owner";
  const apis = useLoad(() => listApis(api), [api]);
  const link = useMemo(grantLink, []);
  const linked = link && apis.state.status === "ready" ? apis.state.data.find((d) => d.id === link.api || d.slug === link.api) : undefined;

  return (
    <div data-theme={theme} className="flex flex-col gap-8 text-slate-900 dark:text-slate-100">
      <header>
        <h1 className="text-xl font-semibold">API</h1>
        <p className="mt-1 text-sm text-slate-500 dark:text-slate-400">
          REST and GraphQL APIs over catalog datasets, read with API keys issued here. Owners can let apps write to them.
        </p>
      </header>


      {link && apis.state.status === "ready" && (
        <div role="status" className="rounded-md border border-indigo-200 bg-indigo-50 px-4 py-3 text-sm text-indigo-900 dark:border-indigo-900 dark:bg-indigo-950 dark:text-indigo-100">
          {!linked ? (
            <>
              A Streamlit app asked to write to an API named <span className="font-mono">{link.api}</span>, but there is no such API in this workspace.
            </>
          ) : isOwner ? (
            <>
              Grant <span className="font-mono">{link.principal || "the app"}</span> writes to {linked.datasetName}: check the form under its Writes below.
            </>
          ) : (
            <>
              A Streamlit app asked to write to {linked.datasetName}. Only a workspace owner can grant that; ask one to open this page.
            </>
          )}
        </div>
      )}
      <ApisSection api={api} state={apis.state} reload={apis.reload} canWrite={canWrite} isOwner={isOwner} link={linked ? link : null} />
      {canWrite && apis.state.status === "ready" && <KeysSection api={api} apis={apis.state.data} />}
    </div>
  );
}

// ---- APIs ---------------------------------------------------------------------------------------

function ApisSection({
  api,
  state,
  reload,
  canWrite,
  isOwner,
  link,
}: {
  api: ApiContext;
  state: LoadState<ApiDefinition[]>;
  reload: () => void;
  canWrite: boolean;
  isOwner: boolean;
  link: GrantLink | null;
}) {
  return (
    <section aria-labelledby="apis-heading" className="flex flex-col gap-3">
      <h2 id="apis-heading" className="text-base font-semibold">
        APIs
      </h2>
      {canWrite && state.status === "ready" && <GenerateForm api={api} existing={state.data} onDone={reload} />}
      {state.status === "loading" && <Muted>Loading APIs…</Muted>}
      {state.status === "error" && <ErrorBanner message={state.error} />}
      {state.status === "ready" &&
        (state.data.length === 0 ? (
          <Muted>No APIs yet.{canWrite ? " Generate one from a catalog dataset above." : ""}</Muted>
        ) : (
          <div className="flex flex-col gap-3">
            {state.data.map((d) => (
              <ApiCard
                key={d.id}
                api={api}
                def={d}
                canWrite={canWrite}
                isOwner={isOwner}
                onChange={reload}
                prefill={link && (link.api === d.id || link.api === d.slug) ? link.principal : undefined}
              />
            ))}
          </div>
        ))}
    </section>
  );
}

function GenerateForm({ api, existing, onDone }: { api: ApiContext; existing: ApiDefinition[]; onDone: () => void }) {
  const datasets = useLoad(() => listPostgresDatasets(api), [api]);
  const [choice, setChoice] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<{ message: string; mismatches?: Mismatch[] } | null>(null);
  const taken = new Set(existing.map((d) => d.datasetId));
  const available = datasets.state.status === "ready" ? datasets.state.data.filter((d) => !taken.has(d.id)) : [];

  async function submit(e: FormEvent) {
    e.preventDefault();
    if (!choice) return;
    setBusy(true);
    setError(null);
    try {
      await generateApi(api, choice);
      setChoice("");
      onDone();
    } catch (err) {
      setError(err instanceof ApiError ? { message: err.message, mismatches: err.mismatches } : { message: String(err) });
    } finally {
      setBusy(false);
    }
  }

  return (
    <Panel>
      <form onSubmit={submit} className="flex flex-wrap items-end gap-3">
        <label className="flex flex-col gap-1 text-sm">
          <span className="font-medium">Generate an API from a dataset</span>
          <select
            className="min-w-64 rounded-md border border-slate-300 bg-white px-2 py-1.5 text-sm dark:border-slate-700 dark:bg-slate-800"
            value={choice}
            onChange={(e) => setChoice(e.target.value)}
            disabled={datasets.state.status !== "ready" || available.length === 0}
          >
            <option value="">
              {datasets.state.status === "loading" ? "Loading datasets…" : available.length === 0 ? "No eligible datasets" : "Choose a dataset"}
            </option>
            {available.map((d) => (
              <option key={d.id} value={d.id}>
                {d.name}
              </option>
            ))}
          </select>
        </label>
        <Button type="submit" disabled={!choice || busy} primary>
          {busy ? "Generating…" : "Generate"}
        </Button>
      </form>
      <p className="mt-2 text-xs text-slate-500 dark:text-slate-400">
        Only catalog datasets that are tables in this workspace&apos;s database are listed.
      </p>
      {datasets.state.status === "error" && <ErrorBanner message={`Couldn't load catalog datasets: ${datasets.state.error}`} />}
      {error && <ErrorBanner message={error.message} mismatches={error.mismatches} />}
    </Panel>
  );
}

function ApiCard({
  api,
  def,
  canWrite,
  isOwner,
  onChange,
  prefill,
}: {
  api: ApiContext;
  def: ApiDefinition;
  canWrite: boolean;
  isOwner: boolean;
  onChange: () => void;
  /** Set when the owner arrived from booth-streamlit's link for this API: the app's principal. */
  prefill?: string;
}) {
  const [busy, setBusy] = useState(false);
  const [confirmDelete, setConfirmDelete] = useState(false);
  const [error, setError] = useState<{ message: string; mismatches?: Mismatch[] } | null>(null);

  async function act(fn: () => Promise<unknown>) {
    setBusy(true);
    setError(null);
    try {
      await fn();
      onChange();
    } catch (err) {
      setError(err instanceof ApiError ? { message: err.message, mismatches: err.mismatches } : { message: String(err) });
    } finally {
      setBusy(false);
      setConfirmDelete(false);
    }
  }

  return (
    <Panel>
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <p className="font-medium">
            {def.datasetName}
            {def.writes && (
              <span className="ml-2 rounded bg-amber-100 px-1.5 py-0.5 text-xs font-medium text-amber-900 dark:bg-amber-900 dark:text-amber-100">
                Writable by apps
              </span>
            )}
          </p>
          <p className="mt-0.5 font-mono text-xs text-slate-500 dark:text-slate-400">
            /{def.slug} · {def.table.schema}.{def.table.name}
          </p>
        </div>
        {canWrite && (
          <div className="flex gap-2">
            <Button disabled={busy} onClick={() => act(() => regenerateApi(api, def.id))}>
              Regenerate
            </Button>
            {confirmDelete ? (
              <>
                <Button danger disabled={busy} onClick={() => act(() => deleteApi(api, def.id))}>
                  Confirm delete
                </Button>
                <Button disabled={busy} onClick={() => setConfirmDelete(false)}>
                  Cancel
                </Button>
              </>
            ) : (
              <Button disabled={busy} onClick={() => setConfirmDelete(true)}>
                Delete
              </Button>
            )}
          </div>
        )}
      </div>
      <table className="mt-3 w-full text-left text-sm">
        <thead className="text-xs uppercase tracking-wide text-slate-500 dark:text-slate-400">
          <tr>
            <th className="py-1 pr-4 font-medium">Column</th>
            <th className="py-1 pr-4 font-medium">Type</th>
            <th className="py-1 font-medium">Description</th>
          </tr>
        </thead>
        <tbody>
          {def.columns.map((c) => (
            <tr key={c.name} className="border-t border-slate-100 dark:border-slate-800">
              <td className="py-1 pr-4 font-mono text-xs">
                {c.name}
                {def.primaryKey.includes(c.name) && <span className="ml-1.5 text-indigo-600 dark:text-indigo-400">key</span>}
              </td>
              <td className="py-1 pr-4 font-mono text-xs text-slate-600 dark:text-slate-300">
                {c.type}
                {c.nullable ? "" : " not null"}
              </td>
              <td className="py-1 text-slate-600 dark:text-slate-300">{c.description}</td>
            </tr>
          ))}
        </tbody>
      </table>
      <p className="mt-2 text-xs text-slate-500 dark:text-slate-400">
        Columns as of {formatDate(def.generatedAt)}. The API keeps these until you regenerate it.
      </p>
      <Endpoints slug={def.slug} />
      <SchemaToggle api={api} id={def.id} generatedAt={def.generatedAt} />
      <WritesToggle api={api} def={def} isOwner={isOwner} onChange={onChange} prefill={prefill} />
      {error && <ErrorBanner message={error.message} mismatches={error.mismatches} />}
    </Panel>
  );
}

// ---- writes (ADR 0117) --------------------------------------------------------------------------

/**
 * booth-streamlit's "Write targets" panel links here so an owner can grant an app (ADR 0117 item 1a;
 * docs/contracts/app-writes.md): /apis?writeApi=<API slug or id>&writePrincipal=streamlit:<ws>:<appId>.
 */
export interface GrantLink {
  api: string;
  principal: string;
}

function grantLink(): GrantLink | null {
  const q = new URLSearchParams(window.location.search);
  const api = q.get("writeApi");
  if (!api) return null;
  return { api, principal: q.get("writePrincipal") ?? "" };
}

const OPERATIONS: { op: WriteOperation; label: string }[] = [
  { op: "insert", label: "Add rows" },
  { op: "update", label: "Change rows" },
  { op: "delete", label: "Delete rows" },
];

function WritesToggle({
  api,
  def,
  isOwner,
  onChange,
  prefill,
}: {
  api: ApiContext;
  def: ApiDefinition;
  isOwner: boolean;
  onChange: () => void;
  prefill?: string;
}) {
  const [open, setOpen] = useState(prefill !== undefined && isOwner);
  if (!isOwner && !def.writes) return null;
  return (
    <div className="mt-2">
      <button
        type="button"
        className="text-xs font-medium text-indigo-700 hover:underline dark:text-indigo-400"
        aria-expanded={open}
        onClick={() => setOpen(!open)}
      >
        {open ? "Hide writes" : def.writes ? "Writes" : "Let apps write…"}
      </button>
      {/* the settings version in the key reloads after a save elsewhere */}
      {open && (
        <WritesPanel key={`${def.generatedAt}@${def.writes?.version ?? 0}`} api={api} def={def} isOwner={isOwner} onChange={onChange} prefill={prefill} />
      )}
    </div>
  );
}

function WritesPanel({
  api,
  def,
  isOwner,
  onChange,
  prefill,
}: {
  api: ApiContext;
  def: ApiDefinition;
  isOwner: boolean;
  onChange: () => void;
  prefill?: string;
}) {
  const view = useLoad(() => getWrites(api, def.id), [api, def.id]);
  if (view.state.status === "loading") return <Muted>Loading write settings…</Muted>;
  if (view.state.status === "error") return <ErrorBanner message={view.state.error} />;
  const v = view.state.data;
  return (
    <div className="mt-2 flex flex-col gap-3 rounded border border-slate-200 p-3 dark:border-slate-800">
      {isOwner ? (
        <WritesForm api={api} def={def} view={v} onSaved={onChange} />
      ) : (
        <p className="text-sm">
          Apps an owner grants can {describe(v.writes?.operations ?? [])} in{" "}
          <span className="font-mono">
            {def.table.schema}.{def.table.name}
          </span>
          , columns {(v.writes?.columns ?? []).join(", ")}. Only workspace owners change this.
        </p>
      )}
      {isOwner && v.writes && <GrantsSection api={api} def={def} writes={v.writes} prefill={prefill} />}
      {isOwner && !v.writes && prefill !== undefined && <Muted>Turn writes on first; then grant the app below.</Muted>}
    </div>
  );
}

function describe(ops: WriteOperation[]): string {
  const words = ops.map((o) => ({ insert: "add rows", update: "change rows", delete: "delete rows" })[o]);
  return words.length === 0 ? "write nothing" : words.join(", ").replace(/, ([^,]*)$/, " and $1");
}

function WritesForm({ api, def, view, onSaved }: { api: ApiContext; def: ApiDefinition; view: WritesView; onSaved: () => void }) {
  const [ops, setOps] = useState<Set<WriteOperation>>(new Set(view.writes?.operations ?? ["insert"]));
  const [cols, setCols] = useState<Set<string>>(new Set(view.writes?.columns ?? view.columns.filter((c) => c.required && c.writable).map((c) => c.column)));
  const [confirmed, setConfirmed] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<{ message: string; problems?: { field: string; problem: string }[] } | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  if (view.needsRegenerate) return <Muted>This API was generated before writes existed. Regenerate it, then turn writes on.</Muted>;
  if (!view.dataAccess) return <Muted>Writes need booth-api&apos;s data access (core.url), which isn&apos;t configured.</Muted>;

  const writesValues = ops.has("insert") || ops.has("update");
  const flip = <T,>(set: Set<T>, item: T) => {
    const next = new Set(set);
    if (next.has(item)) next.delete(item);
    else next.add(item);
    setConfirmed(false);
    return next;
  };
  const chosenCols = writesValues ? view.columns.filter((c) => cols.has(c.column)).map((c) => c.column) : [];
  const chosenOps = OPERATIONS.map((o) => o.op).filter((o) => ops.has(o));

  async function run(fn: () => Promise<unknown>, done?: string) {
    setBusy(true);
    setError(null);
    setNotice(null);
    try {
      await fn();
      if (done) setNotice(done);
      onSaved();
    } catch (err) {
      setError(err instanceof ApiError ? { message: err.message, problems: err.problems } : { message: String(err) });
    } finally {
      setBusy(false);
    }
  }

  return (
    <form
      className="flex flex-col gap-3 text-sm"
      onSubmit={(e) => {
        e.preventDefault();
        void run(() => saveWrites(api, def.id, chosenOps, chosenCols));
      }}
    >
      <div className="flex flex-col gap-1 rounded bg-slate-50 p-2 text-slate-700 dark:bg-slate-950 dark:text-slate-300">
        <p className="font-medium">What these settings mean</p>
        {view.writes ? (
          <p>
            Writes are on. Apps you grant can {describe(view.writes.operations)} in{" "}
            <span className="font-mono">
              {def.table.schema}.{def.table.name}
            </span>
            {view.writes.columns.length > 0 ? `, and can set only these columns: ${view.writes.columns.join(", ")}` : ""}.
          </p>
        ) : (
          <p>
            Writes are off: no app can change{" "}
            <span className="font-mono">
              {def.table.schema}.{def.table.name}
            </span>{" "}
            through this API.
          </p>
        )}
        <p>
          Turning writes on doesn&apos;t let any app write by itself. Each app also needs its own grant, made under &quot;Grant an app&quot; here,
          and its owner must declare this API in the app&apos;s settings page in Streamlit (Write targets), which links back here with the app filled
          in. Anyone who can open a granted app can then write through it, viewers included. Only workspace owners can change these settings or
          grant apps.
        </p>
      </div>
      <fieldset className="flex flex-wrap gap-4">
        <legend className="mb-1 font-medium">What apps may do</legend>
        {OPERATIONS.map(({ op, label }) => (
          <label key={op} className="flex items-center gap-2">
            <input type="checkbox" checked={ops.has(op)} disabled={op !== "insert" && !view.hasPrimaryKey} onChange={() => setOps(flip(ops, op))} />
            {label}
          </label>
        ))}
      </fieldset>
      {writesValues && (
        <fieldset className="flex flex-col gap-1">
          <legend className="mb-1 font-medium">Columns apps may set</legend>
          {view.columns.map((c) => (
            <label key={c.column} className="flex items-center gap-2">
              <input type="checkbox" checked={cols.has(c.column)} disabled={!c.writable} onChange={() => setCols(flip(cols, c.column))} />
              <span className="font-mono text-xs">{c.column}</span>
              <span className="text-xs text-slate-500 dark:text-slate-400">
                {c.type}
                {c.required ? " · required to add a row" : ""}
                {c.reason ? ` · ${c.reason}` : ""}
              </span>
            </label>
          ))}
        </fieldset>
      )}
      <label className="flex items-start gap-2 rounded bg-amber-50 p-2 text-amber-900 dark:bg-amber-950 dark:text-amber-100">
        <input type="checkbox" checked={confirmed} onChange={() => setConfirmed(!confirmed)} />
        <span>
          Apps I grant can {describe(chosenOps)} in{" "}
          <span className="font-mono">
            {def.table.schema}.{def.table.name}
          </span>
          {writesValues ? `, setting ${chosenCols.length === 0 ? "no columns" : chosenCols.join(", ")}` : ""}. Anyone who can open such an app can use
          it to write.
        </span>
      </label>
      <div className="flex gap-2">
        <Button type="submit" primary disabled={busy || !confirmed || chosenOps.length === 0}>
          {view.writes ? "Save" : "Turn writes on"}
        </Button>
        {view.writes && (
          <Button danger disabled={busy} onClick={() => void run(() => disableWrites(api, def.id), "Writes are off, and every grant was revoked.")}>
            Turn writes off
          </Button>
        )}
      </div>
      {notice && <p className="text-xs text-emerald-700 dark:text-emerald-400">{notice}</p>}
      {error && (
        <div role="alert" className="rounded-md border border-red-200 bg-red-50 px-3 py-2 text-sm text-red-800 dark:border-red-900 dark:bg-red-950 dark:text-red-200">
          {error.message}
          {error.problems && (
            <ul className="mt-1 list-disc pl-5">
              {error.problems.map((p) => (
                <li key={p.field}>
                  <span className="font-mono">{p.field}</span>: {p.problem}
                </li>
              ))}
            </ul>
          )}
        </div>
      )}
    </form>
  );
}

function GrantsSection({ api, def, writes, prefill }: { api: ApiContext; def: ApiDefinition; writes: WriteSettings; prefill?: string }) {
  const grants = useLoad(() => listGrants(api, def.id), [api, def.id]);
  const [error, setError] = useState<string | null>(null);
  return (
    <div className="flex flex-col gap-3 text-sm">
      <GrantForm api={api} def={def} writes={writes} prefill={prefill} onGranted={grants.reload} />
      <GrantsList api={api} state={grants.state} reload={grants.reload} error={error} setError={setError} />
    </div>
  );
}

const PRINCIPAL_HINT = "streamlit:<workspace>:<appId>";

function GrantForm({
  api,
  def,
  writes,
  prefill,
  onGranted,
}: {
  api: ApiContext;
  def: ApiDefinition;
  writes: WriteSettings;
  prefill?: string;
  onGranted: () => void;
}) {
  const [principal, setPrincipal] = useState(prefill ?? "");
  const [ops, setOps] = useState<Set<WriteOperation>>(new Set(writes.operations.includes("insert") ? ["insert"] : writes.operations.slice(0, 1)));
  const [narrow, setNarrow] = useState(false);
  const [cols, setCols] = useState<Set<string>>(new Set(writes.columns));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [done, setDone] = useState<string | null>(null);
  const writesValues = ops.has("insert") || ops.has("update");
  const chosenOps = OPERATIONS.map((o) => o.op).filter((o) => ops.has(o));
  const valid = principal.trim() !== "" && chosenOps.length > 0 && (!narrow || !writesValues || cols.size > 0);
  const toggle = <T,>(set: Set<T>, item: T, apply: (s: Set<T>) => void) => {
    const next = new Set(set);
    if (next.has(item)) next.delete(item);
    else next.add(item);
    apply(next);
  };

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(null);
    setDone(null);
    try {
      const g = await createGrant(api, def.id, principal.trim(), chosenOps, narrow && writesValues ? writes.columns.filter((c) => cols.has(c)) : null);
      setDone(`Granted. ${g.principal} can now ${describe(g.operations)} through this API.`);
      onGranted();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <form onSubmit={submit} className="flex flex-col gap-2 rounded bg-slate-50 p-2 dark:bg-slate-950">
      <p className="font-medium">Grant an app</p>
      <div className="flex flex-col gap-1">
        <label htmlFor={`principal-${def.id}`}>App</label>
        <input
          id={`principal-${def.id}`}
          className="max-w-md rounded-md border border-slate-300 bg-white px-2 py-1.5 font-mono text-xs dark:border-slate-700 dark:bg-slate-800"
          value={principal}
          onChange={(e) => setPrincipal(e.target.value)}
          placeholder={PRINCIPAL_HINT}
          aria-describedby={`principal-help-${def.id}`}
        />
        <span id={`principal-help-${def.id}`} className="text-xs text-slate-500 dark:text-slate-400">
          The app&apos;s full id, {PRINCIPAL_HINT}, shown in its settings in Streamlit (Write targets). Granting the same app again replaces its grant.
        </span>
      </div>
      <fieldset className="flex flex-wrap gap-4">
        <legend className="mb-1">It may</legend>
        {OPERATIONS.filter((o) => writes.operations.includes(o.op)).map(({ op, label }) => (
          <label key={op} className="flex items-center gap-2">
            <input type="checkbox" checked={ops.has(op)} onChange={() => toggle(ops, op, setOps)} />
            {label}
          </label>
        ))}
      </fieldset>
      {writesValues && (
        <div className="flex flex-col gap-1">
          <label className="flex items-center gap-2">
            <input type="checkbox" checked={narrow} onChange={() => setNarrow(!narrow)} />
            Only some of the writable columns
          </label>
          {narrow &&
            writes.columns.map((c) => (
              <label key={c} className="ml-6 flex items-center gap-2">
                <input type="checkbox" checked={cols.has(c)} onChange={() => toggle(cols, c, setCols)} />
                <span className="font-mono text-xs">{c}</span>
              </label>
            ))}
          {!narrow && <span className="text-xs text-slate-500 dark:text-slate-400">It can set every writable column: {writes.columns.join(", ")}.</span>}
        </div>
      )}
      <div className="flex flex-wrap items-center gap-3">
        <Button type="submit" primary disabled={busy || !valid}>
          {busy ? "Granting…" : "Grant"}
        </Button>
        {!valid && <span className="text-xs text-slate-500 dark:text-slate-400">Enter the app&apos;s id and tick at least one action</span>}
      </div>
      {done && <p className="text-xs text-emerald-700 dark:text-emerald-400">{done}</p>}
      {error && <ErrorBanner message={error} />}
    </form>
  );
}

function GrantsList({
  api,
  state,
  reload,
  error,
  setError,
}: {
  api: ApiContext;
  state: LoadState<WriteGrant[]>;
  reload: () => void;
  error: string | null;
  setError: (e: string | null) => void;
}) {
  if (state.status === "loading") return <Muted>Loading grants…</Muted>;
  if (state.status === "error") return <ErrorBanner message={state.error} />;
  return (
    <div className="flex flex-col gap-1">
      <p className="font-medium">Apps that may write</p>
      {state.data.length === 0 ? (
        <Muted>None yet.</Muted>
      ) : (
        <ul className="flex flex-col gap-1">
          {state.data.map((g) => (
            <li key={g.id} className="flex flex-wrap items-center justify-between gap-2">
              <span>
                <span className="font-mono text-xs">{g.principal}</span>{" "}
                <span className="text-xs text-slate-500 dark:text-slate-400">
                  {describe(g.operations)}
                  {g.columns ? `, columns ${g.columns.join(", ")}` : ""}, granted by {g.grantedByName}
                </span>
              </span>
              <Button
                onClick={() => {
                  setError(null);
                  revokeGrant(api, g.id).then(reload, (err: unknown) => setError(err instanceof Error ? err.message : String(err)));
                }}
              >
                Revoke
              </Button>
            </li>
          ))}
        </ul>
      )}
      {error && <ErrorBanner message={error} />}
    </div>
  );
}

/** Where a key holder calls this API: core's public route for this module (ADR 0101). */
function endpointBase(slug: string, origin: string = window.location.origin): string {
  return `${origin}/modules/api/public/v1/${slug}`;
}

function Endpoints({ slug }: { slug: string }) {
  const base = endpointBase(slug);
  return (
    <dl className="mt-3 grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-xs">
      <dt className="text-slate-500 dark:text-slate-400">REST</dt>
      <dd className="break-all font-mono">GET {base}/rows</dd>
      <dt className="text-slate-500 dark:text-slate-400">GraphQL</dt>
      <dd className="break-all font-mono">POST {base}/graphql</dd>
      <dt className="text-slate-500 dark:text-slate-400">OpenAPI</dt>
      <dd className="break-all font-mono">GET {base}/openapi.json</dd>
      <dt className="text-slate-500 dark:text-slate-400">Auth</dt>
      <dd className="font-mono">Authorization: Bearer booth_ak_…</dd>
    </dl>
  );
}

function SchemaToggle({ api, id, generatedAt }: { api: ApiContext; id: string; generatedAt: string }) {
  const [open, setOpen] = useState<"graphql" | "openapi" | null>(null);
  const toggle = (which: "graphql" | "openapi") => setOpen(open === which ? null : which);
  return (
    <div className="mt-2">
      <div className="flex gap-4">
        <button
          type="button"
          className="text-xs font-medium text-indigo-700 hover:underline dark:text-indigo-400"
          aria-expanded={open === "graphql"}
          onClick={() => toggle("graphql")}
        >
          {open === "graphql" ? "Hide GraphQL schema" : "Show GraphQL schema"}
        </button>
        <button
          type="button"
          className="text-xs font-medium text-indigo-700 hover:underline dark:text-indigo-400"
          aria-expanded={open === "openapi"}
          onClick={() => toggle("openapi")}
        >
          {open === "openapi" ? "Hide OpenAPI document" : "Show OpenAPI document"}
        </button>
      </div>
      {/* generatedAt in the key reloads after a regenerate */}
      {open === "graphql" && <SchemaView key={generatedAt} api={api} id={id} />}
      {open === "openapi" && <OpenAPIView key={generatedAt} api={api} id={id} />}
    </div>
  );
}

function OpenAPIView({ api, id }: { api: ApiContext; id: string }) {
  const { state } = useLoad(() => getOpenAPI(api, id), [api, id]);
  if (state.status === "loading") return <Muted>Loading OpenAPI document…</Muted>;
  if (state.status === "error") return <ErrorBanner message={state.error} />;
  return (
    <pre className="mt-2 max-h-96 overflow-auto rounded bg-slate-50 p-3 font-mono text-xs text-slate-800 dark:bg-slate-950 dark:text-slate-200">
      {JSON.stringify(state.data, null, 2)}
    </pre>
  );
}

function SchemaView({ api, id }: { api: ApiContext; id: string }) {
  const { state } = useLoad(() => getSchema(api, id), [api, id]);
  if (state.status === "loading") return <Muted>Loading schema…</Muted>;
  if (state.status === "error") return <ErrorBanner message={state.error} />;
  return (
    <div className="mt-2 flex flex-col gap-2">
      {state.data.omitted.length > 0 && (
        <p className="text-xs text-amber-800 dark:text-amber-300">
          Not exposed (no faithful GraphQL type): {state.data.omitted.map((o) => `${o.column} (${o.type})`).join(", ")}
        </p>
      )}
      <pre className="max-h-96 overflow-auto rounded bg-slate-50 p-3 font-mono text-xs text-slate-800 dark:bg-slate-950 dark:text-slate-200">{state.data.sdl}</pre>
    </div>
  );
}

// ---- keys ---------------------------------------------------------------------------------------

function KeysSection({ api, apis }: { api: ApiContext; apis: ApiDefinition[] }) {
  const keys = useLoad(() => listKeys(api), [api]);
  const [issued, setIssued] = useState<IssuedKey | null>(null);
  const names = new Map(apis.map((d) => [d.datasetId, d.datasetName]));

  return (
    <section aria-labelledby="keys-heading" className="flex flex-col gap-3">
      <h2 id="keys-heading" className="text-base font-semibold">
        API keys
      </h2>
      <p className="text-sm text-slate-500 dark:text-slate-400">
        A key reads only the datasets it is scoped to. It stops working if it is revoked, and also once the person who created it hasn&apos;t
        signed in to Booth for a while (7 days by default), so keys for long-running jobs should be created by someone who signs in regularly.
      </p>
      {issued ? (
        <IssuedPanel issued={issued} onDone={() => setIssued(null)} />
      ) : (
        <IssueForm
          api={api}
          apis={apis}
          onIssued={(k) => {
            setIssued(k);
            keys.reload();
          }}
        />
      )}
      {keys.state.status === "loading" && <Muted>Loading keys…</Muted>}
      {keys.state.status === "error" && <ErrorBanner message={keys.state.error} />}
      {keys.state.status === "ready" && <KeyTable api={api} keys={keys.state.data} names={names} onChange={keys.reload} />}
    </section>
  );
}

function IssueForm({ api, apis, onIssued }: { api: ApiContext; apis: ApiDefinition[]; onIssued: (k: IssuedKey) => void }) {
  const [name, setName] = useState("");
  const [scope, setScope] = useState<Set<string>>(new Set());
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  if (apis.length === 0) return <Muted>Generate an API first; a key is scoped to one or more of them.</Muted>;

  function toggle(id: string) {
    const next = new Set(scope);
    if (next.has(id)) next.delete(id);
    else next.add(id);
    setScope(next);
  }

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      const k = await issueKey(api, name, [...scope]);
      setName("");
      setScope(new Set());
      onIssued(k);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <Panel>
      <form onSubmit={submit} className="flex flex-col gap-3">
        <label className="flex flex-col gap-1 text-sm">
          <span className="font-medium">New key name</span>
          <input
            className="max-w-sm rounded-md border border-slate-300 bg-white px-2 py-1.5 text-sm dark:border-slate-700 dark:bg-slate-800"
            value={name}
            maxLength={100}
            onChange={(e) => setName(e.target.value)}
            placeholder="e.g. nightly export"
          />
        </label>
        <fieldset className="flex flex-col gap-1 text-sm">
          <legend className="mb-1 font-medium">Datasets it can read</legend>
          {apis.map((d) => (
            <label key={d.datasetId} className="flex items-center gap-2">
              <input type="checkbox" checked={scope.has(d.datasetId)} onChange={() => toggle(d.datasetId)} />
              {d.datasetName}
            </label>
          ))}
        </fieldset>
        <div className="flex flex-wrap items-center gap-3">
          <Button type="submit" primary disabled={busy || name.trim() === "" || scope.size === 0}>
            {busy ? "Creating…" : "Create key"}
          </Button>
          {(name.trim() === "" || scope.size === 0) && (
            <span className="text-xs text-slate-500 dark:text-slate-400">Enter a name and tick at least one dataset</span>
          )}
        </div>
        {error && <ErrorBanner message={error} />}
      </form>
    </Panel>
  );
}

function IssuedPanel({ issued, onDone }: { issued: IssuedKey; onDone: () => void }) {
  const [copied, setCopied] = useState(false);
  return (
    <div role="alert" className="rounded-md border border-amber-300 bg-amber-50 px-4 py-3 dark:border-amber-700 dark:bg-amber-950">
      <p className="text-sm font-medium text-amber-900 dark:text-amber-100">Copy “{issued.name}” now. It won&apos;t be shown again.</p>
      <p className="mt-1 text-xs text-amber-900 dark:text-amber-100">
        It reads data as you: it stops working if you lose access to this workspace or don&apos;t sign in to Booth for 7 days (the default).
      </p>
      <code className="mt-2 block break-all rounded bg-white px-2 py-1.5 font-mono text-xs text-slate-900 dark:bg-slate-900 dark:text-slate-100">{issued.secret}</code>
      <div className="mt-3 flex gap-2">
        <Button
          onClick={() => {
            void navigator.clipboard?.writeText(issued.secret).then(() => setCopied(true));
          }}
        >
          {copied ? "Copied" : "Copy"}
        </Button>
        <Button onClick={onDone}>Done</Button>
      </div>
    </div>
  );
}

function KeyTable({ api, keys, names, onChange }: { api: ApiContext; keys: ApiKey[]; names: Map<string, string>; onChange: () => void }) {
  if (keys.length === 0) return <Muted>No keys yet.</Muted>;
  return (
    <Panel>
      <table className="w-full text-left text-sm">
        <thead className="text-xs uppercase tracking-wide text-slate-500 dark:text-slate-400">
          <tr>
            <th className="py-1 pr-4 font-medium">Name</th>
            <th className="py-1 pr-4 font-medium">Datasets</th>
            <th className="py-1 pr-4 font-medium">Created</th>
            <th className="py-1 pr-4 font-medium">Last used</th>
            <th className="py-1 pr-4 font-medium">Status</th>
            <th className="py-1 font-medium">
              <span className="sr-only">Actions</span>
            </th>
          </tr>
        </thead>
        <tbody>
          {keys.map((k) => (
            <KeyRow key={k.id} api={api} k={k} names={names} onChange={onChange} />
          ))}
        </tbody>
      </table>
    </Panel>
  );
}

function KeyRow({ api, k, names, onChange }: { api: ApiContext; k: ApiKey; names: Map<string, string>; onChange: () => void }) {
  const [confirm, setConfirm] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const scope = k.datasetIds.length === 0 ? "none (its APIs were deleted)" : k.datasetIds.map((id) => names.get(id) ?? id).join(", ");

  async function revoke() {
    setBusy(true);
    setError(null);
    try {
      await revokeKey(api, k.id);
      onChange();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
      setConfirm(false);
    }
  }

  return (
    <tr className="border-t border-slate-100 align-top dark:border-slate-800">
      <td className="py-2 pr-4">
        {k.name}
        <div className="font-mono text-xs text-slate-500 dark:text-slate-400">booth_ak_{k.id}_…</div>
      </td>
      <td className="py-2 pr-4">{scope}</td>
      <td className="py-2 pr-4">
        {formatDate(k.createdAt)}
        <div className="text-xs text-slate-500 dark:text-slate-400">by {k.createdByName}</div>
        {!k.revokedAt && (
          <div className="text-xs text-slate-500 dark:text-slate-400">Works while {k.createdByName} signs in at least every 7 days</div>
        )}
      </td>
      <td className="py-2 pr-4 text-slate-600 dark:text-slate-300">{k.lastUsedAt ? formatDate(k.lastUsedAt) : "Never"}</td>
      <td className="py-2 pr-4">
        {k.revokedAt ? (
          <span className="text-slate-500 dark:text-slate-400">Revoked {formatDate(k.revokedAt)}</span>
        ) : (
          <span className="text-emerald-700 dark:text-emerald-400">Active</span>
        )}
      </td>
      <td className="py-2 text-right">
        {!k.revokedAt &&
          (confirm ? (
            <span className="inline-flex gap-2">
              <Button danger disabled={busy} onClick={revoke}>
                Confirm revoke
              </Button>
              <Button disabled={busy} onClick={() => setConfirm(false)}>
                Cancel
              </Button>
            </span>
          ) : (
            <Button onClick={() => setConfirm(true)}>Revoke</Button>
          ))}
        {error && <div className="mt-1 text-xs text-red-700 dark:text-red-400">{error}</div>}
      </td>
    </tr>
  );
}

// ---- small pieces -------------------------------------------------------------------------------

function formatDate(iso: string): string {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString();
}

function Panel({ children }: { children: ReactNode }) {
  return <div className="rounded-md border border-slate-200 bg-white px-4 py-3 dark:border-slate-800 dark:bg-slate-900">{children}</div>;
}

function Muted({ children }: { children: ReactNode }) {
  return <p className="text-sm text-slate-500 dark:text-slate-400">{children}</p>;
}

function ErrorBanner({ message, mismatches }: { message: string; mismatches?: Mismatch[] }) {
  return (
    <div role="alert" className="mt-2 rounded-md border border-red-200 bg-red-50 px-3 py-2 text-sm text-red-800 dark:border-red-900 dark:bg-red-950 dark:text-red-200">
      {mismatches && mismatches.length > 0 ? (
        <>
          <p>The catalog&apos;s schema for this dataset doesn&apos;t match the table. Fix the catalog entry, then try again:</p>
          <ul className="mt-1 list-disc pl-5">
            {mismatches.map((m) => (
              <li key={m.column}>
                <span className="font-mono">{m.column}</span>: {m.problem}
              </li>
            ))}
          </ul>
        </>
      ) : (
        message
      )}
    </div>
  );
}

function Button({
  children,
  onClick,
  disabled,
  type = "button",
  primary,
  danger,
}: {
  children: ReactNode;
  onClick?: () => void;
  disabled?: boolean;
  type?: "button" | "submit";
  primary?: boolean;
  danger?: boolean;
}) {
  const look = primary
    ? "border-indigo-600 bg-indigo-600 text-white hover:bg-indigo-700"
    : danger
      ? "border-red-600 bg-red-600 text-white hover:bg-red-700"
      : "border-slate-300 text-slate-700 hover:bg-slate-100 dark:border-slate-600 dark:text-slate-300 dark:hover:bg-slate-800";
  return (
    <button
      type={type}
      onClick={onClick}
      disabled={disabled}
      className={`rounded-md border px-3 py-1.5 text-sm font-medium focus:outline-none focus:ring-2 focus:ring-indigo-500 disabled:cursor-not-allowed disabled:opacity-50 ${look}`}
    >
      {children}
    </button>
  );
}
