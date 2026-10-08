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
    expect(calls.find((c) => c.method === "POST")?.body).toEqual({ name: "export", datasetIds: ["ds-orders"] });
    await user.click(screen.getByRole("button", { name: "Done" }));
    expect(screen.queryByText(secret)).not.toBeInTheDocument();
    expect(document.body.textContent).not.toContain("s".repeat(43));
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
});
