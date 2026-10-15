import type { ApiDefinition, ApiKey, CatalogDataset, GraphQLSchema, IssuedKey, Mismatch, WriteGrant, WriteOperation, WritesView } from "../types";

// Mounted inside booth-design's shell, so requests resolve against the shell's origin and go
// through booth-core's gateway at /modules/{id}/*, which strips the prefix before forwarding. This
// module's own routes live under /api/, so its base is /modules/api/api. A bare "/api/..." would
// hit booth-core's own API instead.
const BASE = "/modules/api/api";
const CATALOG = "/modules/catalog/api";

export type GetAccessToken = () => string | null;

/** Everything a request needs from the mounting shell (ADR 0031/0033). */
export interface ApiContext {
  workspace: string;
  getAccessToken: GetAccessToken;
}

export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
    public mismatches?: Mismatch[],
    public problems?: { field: string; problem: string }[],
  ) {
    super(message);
    this.name = "ApiError";
  }
}

// X-Workspace is required by the gateway on every authenticated request (ADR 0025).
// getAccessToken is called fresh before each request, never cached (ADR 0033); a null token omits
// the header rather than sending "Bearer null".
async function request<T>(ctx: ApiContext, url: string, init: { method?: string; body?: unknown } = {}): Promise<T> {
  const headers = new Headers({ "X-Workspace": ctx.workspace });
  const token = ctx.getAccessToken();
  if (token !== null) headers.set("Authorization", `Bearer ${token}`);
  if (init.body !== undefined) headers.set("Content-Type", "application/json");
  const res = await fetch(url, {
    method: init.method ?? "GET",
    headers,
    body: init.body === undefined ? undefined : JSON.stringify(init.body),
  });
  if (!res.ok) {
    const text = await res.text();
    let message = text || `HTTP ${res.status}`;
    let mismatches: Mismatch[] | undefined;
    let problems: { field: string; problem: string }[] | undefined;
    try {
      const body = JSON.parse(text) as { error?: string; mismatches?: Mismatch[]; problems?: { field: string; problem: string }[] };
      if (body.error) message = body.error;
      mismatches = body.mismatches;
      problems = body.problems;
    } catch {
      // not JSON — e.g. a gateway error page
    }
    throw new ApiError(res.status, message, mismatches, problems);
  }
  if (res.status === 204) return undefined as T;
  return (await res.json()) as T;
}

export const listApis = (ctx: ApiContext) => request<{ items: ApiDefinition[] }>(ctx, `${BASE}/apis`).then((r) => r.items);

export const generateApi = (ctx: ApiContext, datasetId: string) =>
  request<ApiDefinition>(ctx, `${BASE}/apis`, { method: "POST", body: { datasetId } });

export const getSchema = (ctx: ApiContext, id: string) => request<GraphQLSchema>(ctx, `${BASE}/apis/${encodeURIComponent(id)}/schema`);

export const getOpenAPI = (ctx: ApiContext, id: string) => request<Record<string, unknown>>(ctx, `${BASE}/apis/${encodeURIComponent(id)}/openapi`);

export const regenerateApi = (ctx: ApiContext, id: string) =>
  request<ApiDefinition>(ctx, `${BASE}/apis/${encodeURIComponent(id)}/regenerate`, { method: "POST" });

export const deleteApi = (ctx: ApiContext, id: string) => request<void>(ctx, `${BASE}/apis/${encodeURIComponent(id)}`, { method: "DELETE" });

export const listKeys = (ctx: ApiContext) => request<{ items: ApiKey[] }>(ctx, `${BASE}/keys`).then((r) => r.items);

export const issueKey = (ctx: ApiContext, name: string, datasetIds: string[]) =>
  request<IssuedKey>(ctx, `${BASE}/keys`, { method: "POST", body: { name, datasetIds } });

export const revokeKey = (ctx: ApiContext, id: string) =>
  request<ApiKey>(ctx, `${BASE}/keys/${encodeURIComponent(id)}/revoke`, { method: "POST" });

// ---- writes (ADR 0117; docs/contracts/app-writes.md). Changing anything needs the owner role. ----

export const getWrites = (ctx: ApiContext, id: string) => request<WritesView>(ctx, `${BASE}/apis/${encodeURIComponent(id)}/writes`);

export const saveWrites = (ctx: ApiContext, id: string, operations: WriteOperation[], columns: string[]) =>
  request<WritesView>(ctx, `${BASE}/apis/${encodeURIComponent(id)}/writes`, { method: "PUT", body: { operations, columns } });

export const disableWrites = (ctx: ApiContext, id: string) =>
  request<{ revokedGrants: number; databaseRevoked: boolean; databaseError?: string }>(ctx, `${BASE}/apis/${encodeURIComponent(id)}/writes`, {
    method: "DELETE",
  });

export const listGrants = (ctx: ApiContext, apiId: string) =>
  request<{ items: WriteGrant[] }>(ctx, `${BASE}/write-grants?apiId=${encodeURIComponent(apiId)}`).then((r) => r.items);

export const createGrant = (ctx: ApiContext, apiId: string, principal: string, operations: WriteOperation[], columns: string[] | null) =>
  request<WriteGrant>(ctx, `${BASE}/write-grants`, { method: "POST", body: { apiId, principal, operations, columns } });

export const revokeGrant = (ctx: ApiContext, id: string) =>
  request<WriteGrant>(ctx, `${BASE}/write-grants/${encodeURIComponent(id)}/revoke`, { method: "POST" });

/** booth-catalog's page size cap (its asset.MaxLimit); pages beyond MAX_PAGES aren't fetched. */
const CATALOG_PAGE = 200;
const MAX_PAGES = 10;

/**
 * The workspace's catalog datasets that v0 can turn into an API (ADR 0102's format "postgres"),
 * read straight from booth-catalog through the gateway as the signed-in person. The catalog has no
 * format filter, so this pages through and filters here.
 */
export async function listPostgresDatasets(ctx: ApiContext): Promise<CatalogDataset[]> {
  const out: CatalogDataset[] = [];
  for (let page = 0; page < MAX_PAGES; page++) {
    const r = await request<{ items: CatalogDataset[]; total: number }>(ctx, `${CATALOG}/datasets?limit=${CATALOG_PAGE}&offset=${page * CATALOG_PAGE}`);
    out.push(...r.items.filter((d) => d.format === "postgres"));
    if ((page + 1) * CATALOG_PAGE >= r.total || r.items.length === 0) break;
  }
  return out;
}
