// Mirrors the JSON shapes in internal/apidef, internal/store and internal/api — hand-written, like
// every other native module's UI package (no shared-schema tooling). Keep in sync by hand.

/** Caller's role in the active workspace (ADR 0025), per the NativeModuleProps contract (ADR 0031). */
export type WorkspaceRole = "owner" | "editor" | "viewer";

export interface Column {
  name: string;
  /** PostgreSQL's own type, from introspection. */
  type: string;
  nullable: boolean;
  description?: string;
}

/** A generated API: the snapshot of one dataset's table (GET /api/apis). */
export interface ApiDefinition {
  id: string;
  datasetId: string;
  datasetName: string;
  slug: string;
  table: { schema: string; name: string };
  columns: Column[];
  primaryKey: string[];
  createdBy: string;
  createdAt: string;
  generatedAt: string;
  /** Snapshots older than 2 predate writes: regenerate before turning writes on. */
  snapshotVersion?: number;
  /** Write settings (ADR 0117); null when writes are off. */
  writes?: WriteSettings | null;
}

export type WriteOperation = "insert" | "update" | "delete";

/** What an owner turned on for an API (ADR 0117). Columns are real column names. */
export interface WriteSettings {
  operations: WriteOperation[];
  columns: string[];
  version: number;
  updatedBy: string;
  updatedAt: string;
}

/** GET /api/apis/{id}/writes. */
export interface WritesView {
  apiId: string;
  writes: WriteSettings | null;
  columns: { column: string; field?: string; type: string; writable: boolean; reason?: string; required: boolean }[];
  hasPrimaryKey: boolean;
  needsRegenerate: boolean;
  dataAccess: boolean;
}

/** A write grant to an app (principal streamlit:<workspace>:<appId>). */
export interface WriteGrant {
  id: string;
  apiId: string;
  principal: string;
  operations: WriteOperation[];
  /** null: every column the API makes writable. */
  columns: string[] | null;
  grantedBy: string;
  grantedByName: string;
  createdAt: string;
  revokedAt: string | null;
}

/** An API key as listed. The secret is never part of this. */
export interface ApiKey {
  id: string;
  name: string;
  createdBy: string;
  createdByName: string;
  createdAt: string;
  revokedAt: string | null;
  revokedBy?: string;
  /** Last request this key authenticated on a public endpoint (recorded at most once a minute). */
  lastUsedAt: string | null;
  datasetIds: string[];
}

/** POST /api/keys: the one response that carries the secret. */
export interface IssuedKey extends ApiKey {
  secret: string;
}

export interface Mismatch {
  column: string;
  problem: string;
}

/** The part of a booth-catalog dataset this view uses. */
export interface CatalogDataset {
  id: string;
  name: string;
  format?: string;
}

/** GET /api/apis/{id}/schema: the GraphQL schema the endpoint will serve. */
export interface GraphQLSchema {
  sdl: string;
  /** Columns left out because their type has no faithful GraphQL mapping (e.g. bytea). */
  omitted: { column: string; type: string }[];
}
