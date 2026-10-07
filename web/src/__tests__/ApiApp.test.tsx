import { render, screen } from "@testing-library/react";
import { ApiApp } from "../ApiApp";

describe("ApiApp", () => {
  it("renders with the ADR 0031/0033 props and makes no requests yet", () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch");
    const getAccessToken = vi.fn(() => "t");
    render(<ApiApp workspace="acme" role="owner" theme="dark" getAccessToken={getAccessToken} />);
    expect(screen.getByRole("heading", { name: "API" })).toBeInTheDocument();
    expect(screen.getByText(/not available yet/i)).toBeInTheDocument();
    expect(fetchSpy).not.toHaveBeenCalled();
    expect(getAccessToken).not.toHaveBeenCalled();
    fetchSpy.mockRestore();
  });
});
