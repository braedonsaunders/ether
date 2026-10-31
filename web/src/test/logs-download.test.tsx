import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";
import { setToken } from "../lib/api";
import Logs from "../pages/Logs";

afterEach(() => setToken(""));

describe("Logs download", () => {
  it("fetches the scrubbed bundle with the console session and saves it", async () => {
    setToken("ether-admin-logs");
    const calls: { url: string; init?: RequestInit }[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = String(input);
        calls.push({ url, init });
        if (url.endsWith("/logs/download"))
          return new Response("# Ether logs\n", { headers: { "Content-Type": "text/plain" } });
        return new Response(JSON.stringify({ entries: [], total: 0 }), {
          headers: { "Content-Type": "application/json" },
        });
      }),
    );
    const createObjectURL = vi.fn(() => "blob:logs");
    Object.assign(URL, { createObjectURL, revokeObjectURL: vi.fn() });
    const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => {});
    render(
      <MemoryRouter>
        <Logs />
      </MemoryRouter>,
    );
    fireEvent.click(await screen.findByRole("button", { name: "Download logs" }));
    await waitFor(() => expect(click).toHaveBeenCalledOnce());
    const req = calls.find((c) => c.url === "/ether/api/logs/download")!;
    // Authenticated like every console request (the header name is the API's).
    expect([...new Headers(req.init?.headers).values()]).toContain("ether-admin-logs");
    expect(createObjectURL).toHaveBeenCalledOnce();
    click.mockRestore();
  });
});
