import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { it, expect, vi } from "vitest";
import { BackupsAndImport } from "../pages/settings/Backups";

it("keeps a selected CSV viewer when an obsolete users response arrives", async () => {
  let oldResolve!: (r: Response) => void;
  let newResolve!: (r: Response) => void;
  let n = 0;
  const calls: string[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((input: RequestInfo | URL) => {
      calls.push(String(input));
      if (String(input).endsWith("/users")) {
        n++;
        return new Promise<Response>((resolve) => {
          if (n === 1) oldResolve = resolve;
          else newResolve = resolve;
        });
      }
      if (String(input).includes("/migration/csv/preview"))
        return Promise.resolve(
          new Response(JSON.stringify({ id: "plan", items: [], lists: [], warnings: [] })),
        );
      return Promise.resolve(new Response("{}"));
    }),
  );
  const first = render(<BackupsAndImport tab="import" />);
  await waitFor(() => expect(n).toBe(1));
  first.unmount();
  render(<BackupsAndImport tab="import" />);
  await waitFor(() => expect(n).toBe(2));
  fireEvent.click(screen.getByRole("button", { name: "CSV viewer" }));
  await act(async () => {
    newResolve(
      new Response(
        JSON.stringify({
          users: [
            { id: 7, title: "Sam" },
            { id: 12, title: "Alex" },
          ],
        }),
      ),
    );
  });
  fireEvent.click(await screen.findByRole("option", { name: "Alex" }));
  expect(screen.getByRole("button", { name: "CSV viewer" })).toHaveTextContent("Alex");
  await act(async () => {
    oldResolve(new Response(JSON.stringify({ users: [{ id: 8, title: "Stale" }] })));
  });
  expect(screen.getByRole("button", { name: "CSV viewer" })).toHaveTextContent("Alex");
  expect(screen.getByRole("button", { name: "Ether viewer" })).toHaveTextContent("Sam");
  fireEvent.change(screen.getByLabelText("Preview a CSV import"), {
    target: { files: [new File(["title\nExample"], "history.csv", { type: "text/csv" })] },
  });
  await waitFor(() =>
    expect(calls.some((url) => url.includes("/migration/csv/preview?viewer=12&"))).toBe(true),
  );
  expect(await screen.findByRole("region", { name: "Review import" })).toBeInTheDocument();
});

it("keeps an early CSV selection when viewer initialization races the click", async () => {
  const { cleanup } = await import("@testing-library/react");
  for (let i = 0; i < 200; i++) {
    const trace: string[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input);
        trace.push("fetch " + url);
        if (url.includes("/migration/csv/preview"))
          return new Response(JSON.stringify({ id: "plan", items: [], lists: [], warnings: [] }));
        return new Response(
          JSON.stringify(
            url.endsWith("/users")
              ? {
                  users: [
                    { id: 7, title: "Sam" },
                    { id: 12, title: "Alex" },
                  ],
                }
              : {},
          ),
        );
      }),
    );
    render(<BackupsAndImport tab="import" />);
    const trigger = screen.getByRole("button", { name: "CSV viewer" });
    trace.push("before-open " + trigger.textContent);
    fireEvent.click(trigger);
    const option = await screen.findByRole("option", { name: "Alex" });
    trace.push(
      "option connected=" +
        option.isConnected +
        " trigger=" +
        trigger.textContent +
        " expanded=" +
        trigger.getAttribute("aria-expanded"),
    );
    fireEvent.click(option);
    trace.push(
      "after-click " + trigger.textContent + " expanded=" + trigger.getAttribute("aria-expanded"),
    );
    if (!trigger.textContent?.includes("Alex"))
      throw new Error("iteration " + i + " " + trace.join(" | "));
    fireEvent.change(screen.getByLabelText("Preview a CSV import"), {
      target: { files: [new File(["title\nExample"], "history.csv", { type: "text/csv" })] },
    });
    await waitFor(() =>
      expect(trace.some((entry) => entry.includes("/migration/csv/preview?viewer=12&"))).toBe(true),
    );
    await screen.findByRole("region", { name: "Review import" });
    cleanup();
    vi.unstubAllGlobals();
  }
}, 60000);
