import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ApiApp } from "../ApiApp";
import type { ApiDefinition, ApiKey } from "../types";

const orders: ApiDefinition = {
  id: "api-1",
  datasetId: "ds-orders",
  datasetName: "Orders",
  slug: "orders",
  table: { schema: "public", name: "orders" },
  columns: [
    { name: "id", type: "bigint", nullable: false },
    { name: "amount", type: "numeric(12,2)", nullable: true, description: "in CAD" },
  ],
  primaryKey: ["id"],
  createdBy: "sub-ed",
  createdAt: "2026-10-07T10:00:00Z",
  generatedAt: "2026-10-07T10:00:00Z",
};

const activeKey: ApiKey = {
  id: "abcdefghijklm",
  name: "nightly",
  createdBy: "sub-ed",
  createdByName: "ed",
  createdAt: "2026-10-07T11:00:00Z",
  revokedAt: null,
  lastUsedAt: null,
  datasetIds: ["ds-orders"],
};

type Call = { url: string; method: string; headers: Headers; body?: unknown };

/** A fake gateway: routes by method + path, records every call. */
function mockFetch(routes: Record<string, (body: unknown) => { status: number; body?: unknown }>) {
  const calls: Call[] = [];
  const fn = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    const method = init?.method ?? "GET";
    const body = init?.body ? JSON.parse(String(init.body)) : undefined;
    calls.push({ url, method, headers: new Headers(init?.headers), body });
    const path = url.split("?")[0];
    const handler = routes[`${method} ${path}`];
    const res = handler ? handler(body) : { status: 404, body: { error: "not found" } };
    return new Response(res.body === undefined ? null : JSON.stringify(res.body), { status: res.status });
  });
  vi.stubGlobal("fetch", fn);
  return calls;
}

afterEach(() => {
  vi.unstubAllGlobals();
});

const base = {
  "GET /modules/api/api/apis": () => ({ status: 200, body: { items: [orders] } }),
  "GET /modules/api/api/keys": () => ({ status: 200, body: { items: [activeKey] } }),
  "GET /modules/catalog/api/datasets": () => ({
    status: 200,
    body: {
      total: 3,
      items: [
        { id: "ds-orders", name: "Orders", format: "postgres" },
        { id: "ds-customers", name: "Customers", format: "postgres" },
        { id: "ds-raw", name: "Raw files", format: "file" },
      ],
    },
  }),
};

describe("ApiApp", () => {
  it("shows an API's GraphQL schema and the columns it leaves out", async () => {
    mockFetch({
      ...base,
      "GET /modules/api/api/apis/api-1/schema": () => ({
        status: 200,
        body: { sdl: "type Row {\n  id: BigInt!\n}", omitted: [{ column: "blob", type: "bytea" }] },
      }),
    });
    const user = userEvent.setup();
    render(<ApiApp workspace="acme" role="viewer" theme="light" getAccessToken={() => "tok"} />);
    await user.click(await screen.findByRole("button", { name: "Show GraphQL schema" }));
    expect(await screen.findByText(/id: BigInt!/)).toBeInTheDocument();
    expect(screen.getByText(/blob \(bytea\)/)).toBeInTheDocument();
  });

  it("shows an API's OpenAPI document", async () => {
    mockFetch({
      ...base,
      "GET /modules/api/api/apis/api-1/openapi": () => ({ status: 200, body: { openapi: "3.1.0", paths: { "/rows": {} } } }),
    });
    const user = userEvent.setup();
    render(<ApiApp workspace="acme" role="viewer" theme="light" getAccessToken={() => "tok"} />);
    await user.click(await screen.findByRole("button", { name: "Show OpenAPI document" }));
    expect(await screen.findByText(/"openapi": "3.1.0"/)).toBeInTheDocument();
  });

  it("shows a viewer the APIs read-only and never asks for keys", async () => {
    const calls = mockFetch(base);
    render(<ApiApp workspace="acme" role="viewer" theme="light" getAccessToken={() => "tok"} />);
    expect(await screen.findByText("Orders")).toBeInTheDocument();
    expect(screen.getByText("in CAD")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /generate/i })).not.toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "API keys" })).not.toBeInTheDocument();
    expect(calls.some((c) => c.url.includes("/keys"))).toBe(false);
    expect(calls.some((c) => c.url.includes("/catalog/"))).toBe(false);
  });

  it("sends X-Workspace and a fresh bearer token, and omits Authorization when there is none", async () => {
    const calls = mockFetch(base);
    let token: string | null = "first";
    const { unmount } = render(<ApiApp workspace="acme" role="viewer" theme="light" getAccessToken={() => token} />);
    await screen.findByText("Orders");
    expect(calls[0].headers.get("X-Workspace")).toBe("acme");
    expect(calls[0].headers.get("Authorization")).toBe("Bearer first");
    unmount();
    token = null;
    render(<ApiApp workspace="acme" role="viewer" theme="light" getAccessToken={() => token} />);
    await waitFor(() => expect(calls.length).toBeGreaterThan(1));
    expect(calls[calls.length - 1].headers.has("Authorization")).toBe(false);
  });

  it("offers only postgres datasets without an API, and shows a catalog/table mismatch", async () => {
    const calls = mockFetch({
      ...base,
      "POST /modules/api/api/apis": () => ({
        status: 409,
        body: { error: "mismatch", mismatches: [{ column: "currency", problem: "in the catalog but not in the table" }] },
      }),
    });
    const user = userEvent.setup();
    render(<ApiApp workspace="acme" role="editor" theme="light" getAccessToken={() => "tok"} />);
    const select = await screen.findByRole("combobox");
    await waitFor(() => expect(within(select).getAllByRole("option")).toHaveLength(2));
    expect(within(select).queryByText("Orders")).not.toBeInTheDocument(); // already has an API
    expect(within(select).queryByText("Raw files")).not.toBeInTheDocument(); // not postgres
    await user.selectOptions(select, "ds-customers");
    await user.click(screen.getByRole("button", { name: "Generate" }));
    expect(await screen.findByText("currency")).toBeInTheDocument();
    expect(calls.find((c) => c.method === "POST")?.body).toEqual({ datasetId: "ds-customers" });
  });

  it("shows a new key's secret once, and never again after Done", async () => {
    const secret = "booth_ak_nopqrstuvwxyz_" + "s".repeat(43);
    const calls = mockFetch({
      ...base,
      "POST /modules/api/api/keys": () => ({ status: 201, body: { ...activeKey, id: "nopqrstuvwxyz", name: "export", secret } }),
    });
    const user = userEvent.setup();
    render(<ApiApp workspace="acme" role="owner" theme="dark" getAccessToken={() => "tok"} />);
    await user.type(await screen.findByLabelText("New key name"), "export");
    await user.click(screen.getByRole("checkbox", { name: "Orders" }));
    await user.click(screen.getByRole("button", { name: "Create key" }));
    expect(await screen.findByText(secret)).toBeInTheDocument();
    expect(screen.getByText(/don.t sign in to Booth for 7 days/)).toBeInTheDocument();
    expect(calls.find((c) => c.method === "POST")?.body).toEqual({ name: "export", datasetIds: ["ds-orders"] });
    await user.click(screen.getByRole("button", { name: "Done" }));
    expect(screen.queryByText(secret)).not.toBeInTheDocument();
    expect(document.body.textContent).not.toContain("s".repeat(43));
  });

  it("shows where to call each API and how long its keys keep working", async () => {
    mockFetch({
      ...base,
      "GET /modules/api/api/keys": () => ({ status: 200, body: { items: [{ ...activeKey, lastUsedAt: "2026-10-08T09:00:00Z" }] } }),
    });
    render(<ApiApp workspace="acme" role="owner" theme="light" getAccessToken={() => "tok"} />);
    expect(await screen.findByText(`POST ${window.location.origin}/modules/api/public/v1/orders/graphql`)).toBeInTheDocument();
    expect(screen.getByText(`GET ${window.location.origin}/modules/api/public/v1/orders/rows`)).toBeInTheDocument();
    expect(await screen.findByText("Works while ed signs in at least every 7 days")).toBeInTheDocument();
    expect(screen.queryByText("Never")).not.toBeInTheDocument();
  });

  it("says why Create key is disabled until there is a name and a dataset", async () => {
    mockFetch(base);
    const user = userEvent.setup();
    render(<ApiApp workspace="acme" role="editor" theme="light" getAccessToken={() => "tok"} />);
    const create = await screen.findByRole("button", { name: "Create key" });
    expect(create).toBeDisabled();
    expect(screen.getByText("Enter a name and tick at least one dataset")).toBeInTheDocument();
    await user.type(screen.getByLabelText("New key name"), "export");
    await user.click(screen.getByRole("checkbox", { name: "Orders" }));
    expect(create).toBeEnabled();
    expect(screen.queryByText("Enter a name and tick at least one dataset")).not.toBeInTheDocument();
  });

  it("revokes a key only after confirming", async () => {
    let revoked = false;
    const calls = mockFetch({
      ...base,
      "GET /modules/api/api/keys": () => ({ status: 200, body: { items: [revoked ? { ...activeKey, revokedAt: "2026-10-07T12:00:00Z" } : activeKey] } }),
      "POST /modules/api/api/keys/abcdefghijklm/revoke": () => {
        revoked = true;
        return { status: 200, body: { ...activeKey, revokedAt: "2026-10-07T12:00:00Z" } };
      },
    });
    const user = userEvent.setup();
    render(<ApiApp workspace="acme" role="editor" theme="light" getAccessToken={() => "tok"} />);
    await user.click(await screen.findByRole("button", { name: "Revoke" }));
    expect(calls.some((c) => c.url.endsWith("/revoke"))).toBe(false);
    await user.click(screen.getByRole("button", { name: "Confirm revoke" }));
    expect(await screen.findByText(/^Revoked/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Revoke" })).not.toBeInTheDocument();
  });

  describe("writes (ADR 0117)", () => {
    const view = (writes: unknown) => ({
      status: 200,
      body: {
        apiId: "api-1",
        writes,
        hasPrimaryKey: true,
        needsRegenerate: false,
        dataAccess: true,
        columns: [
          { column: "id", field: "id", type: "bigint", writable: false, reason: "a primary-key column the database fills in", required: false },
          { column: "amount", field: "amount", type: "numeric(12,2)", writable: true, required: true },
        ],
      },
    });
    const on = { operations: ["insert"], columns: ["amount"], version: 1, updatedBy: "alice", updatedAt: "2026-10-15T00:00:00Z" };

    it("lets an owner turn writes on only after confirming in plain words", async () => {
      const calls = mockFetch({
        ...base,
        "GET /modules/api/api/apis/api-1/writes": () => view(null),
        "PUT /modules/api/api/apis/api-1/writes": () => view(on),
      });
      const user = userEvent.setup();
      render(<ApiApp workspace="acme" role="owner" theme="light" getAccessToken={() => "tok"} />);
      await user.click(await screen.findByRole("button", { name: "Let apps write…" }));
      expect(await screen.findByLabelText("Add rows")).toBeChecked();
      expect(screen.getByText(/Writes are off: no app can change/)).toBeInTheDocument();
      expect(screen.getByText(/Each app also needs a grant, made from that app's settings page in Streamlit/)).toBeInTheDocument();
      expect(screen.getByText(/a primary-key column the database fills in/)).toBeInTheDocument();
      const save = screen.getByRole("button", { name: "Turn writes on" });
      expect(save).toBeDisabled();
      await user.click(screen.getByText(/Apps I grant can add rows in/));
      await user.click(save);
      await waitFor(() => expect(calls.some((c) => c.method === "PUT")).toBe(true));
      expect(calls.find((c) => c.method === "PUT")?.body).toEqual({ operations: ["insert"], columns: ["amount"] });
    });

    it("never offers writes to an editor, and shows a viewer what a writable API allows", async () => {
      mockFetch(base);
      const { unmount } = render(<ApiApp workspace="acme" role="editor" theme="light" getAccessToken={() => "tok"} />);
      await screen.findByText("/orders · public.orders");
      expect(screen.queryByRole("button", { name: "Let apps write…" })).not.toBeInTheDocument();
      unmount();
      mockFetch({
        ...base,
        "GET /modules/api/api/apis": () => ({ status: 200, body: { items: [{ ...orders, writes: on }] } }),
        "GET /modules/api/api/apis/api-1/writes": () => view(on),
      });
      const user = userEvent.setup();
      render(<ApiApp workspace="acme" role="viewer" theme="light" getAccessToken={() => "tok"} />);
      expect(await screen.findByText("Writable by apps")).toBeInTheDocument();
      await user.click(screen.getByRole("button", { name: "Writes" }));
      expect(await screen.findByText(/Only workspace owners change this/)).toBeInTheDocument();
      expect(screen.queryByRole("button", { name: "Turn writes off" })).not.toBeInTheDocument();
    });

    it("lets an owner revoke an app's grant and turn writes off", async () => {
      const grant = {
        id: "g1", apiId: "api-1", principal: "streamlit:acme:budget", operations: ["insert"], columns: null,
        grantedBy: "alice", grantedByName: "Alice", createdAt: "2026-10-15T00:00:00Z", revokedAt: null,
      };
      let revoked = false;
      const calls = mockFetch({
        ...base,
        "GET /modules/api/api/apis": () => ({ status: 200, body: { items: [{ ...orders, writes: on }] } }),
        "GET /modules/api/api/apis/api-1/writes": () => view(on),
        "GET /modules/api/api/write-grants": () => ({ status: 200, body: { items: revoked ? [] : [grant] } }),
        "POST /modules/api/api/write-grants/g1/revoke": () => {
          revoked = true;
          return { status: 200, body: { ...grant, revokedAt: "2026-10-15T01:00:00Z" } };
        },
        "DELETE /modules/api/api/apis/api-1/writes": () => ({ status: 200, body: { revokedGrants: 0, databaseRevoked: true } }),
      });
      const user = userEvent.setup();
      render(<ApiApp workspace="acme" role="owner" theme="light" getAccessToken={() => "tok"} />);
      await user.click(await screen.findByRole("button", { name: "Writes" }));
      expect(await screen.findByText("streamlit:acme:budget")).toBeInTheDocument();
      const grantRow = screen.getByText("streamlit:acme:budget").closest("li") as HTMLElement;
      await user.click(within(grantRow).getByRole("button", { name: "Revoke" }));
      expect(await screen.findByText(/None yet/)).toBeInTheDocument();
      await user.click(screen.getByRole("button", { name: "Turn writes off" }));
      await waitFor(() => expect(calls.some((c) => c.method === "DELETE" && c.url.endsWith("/writes"))).toBe(true));
    });
  });
});
