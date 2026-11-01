import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { calls, renderApp } from "./harness";
import { lazy, Suspense } from "react";
import { Drawer } from "../components/Drawer";

const WAIT = { timeout: 4000 };
describe("Owner usability", () => {
  it("contains a cold editor load inside its drawer without hiding the page", async () => {
    let resolve!: (module: { default: () => React.ReactNode }) => void;
    const Editor = lazy(
      () =>
        new Promise<{ default: () => React.ReactNode }>((done) => {
          resolve = done;
        }),
    );
    render(
      <Suspense fallback={<p>Whole page loading</p>}>
        <p>Existing page content</p>
        <Drawer open title="Lazy editor" onClose={() => {}}>
          <Editor />
        </Drawer>
      </Suspense>,
    );
    const page = screen.getByText("Existing page content");
    expect(page).toBeVisible();
    expect(screen.queryByText("Whole page loading")).toBeNull();
    expect(screen.getByRole("status", { name: "Loading editor" })).toBeInTheDocument();
    await act(async () => resolve({ default: () => <p>Editor ready</p> }));
    expect(await screen.findByText("Editor ready", {}, WAIT)).toBeInTheDocument();
    expect(screen.getByText("Existing page content")).toBe(page);
    expect(page).toBeVisible();
  });

  it.each([
    { path: "/sources/usenet", trigger: "Add a Usenet provider", dialog: "New provider" },
    { path: "/sources/debrid", trigger: "Add a debrid service", dialog: "New debrid provider" },
    { path: "/sources/indexers", trigger: "Import from Prowlarr", dialog: "Import from Prowlarr" },
    { path: "/cache", trigger: "Edit policy", dialog: "Edit cache policy" },
    { path: "/notifications/channels", trigger: "New channel", dialog: "New notification channel" },
  ])("keeps $path mounted while its drawer opens and closes", async ({ path, trigger, dialog }) => {
    const { fake } = renderApp(path);
    const control = await screen.findByRole("button", { name: trigger }, WAIT);
    // The import action belongs to the page shell and appears before the
    // indexer list finishes its initial config read. Observe the loaded list
    // before checking that opening its drawer does not remount/refetch it.
    if (path === "/sources/indexers") {
      await screen.findByRole("button", { name: "Add indexer" }, WAIT);
    }
    const page = control.closest("main")!;
    const configReads = calls(fake, "GET", "/config").length;
    fireEvent.click(control);
    const editor = await screen.findByRole("dialog", { name: dialog }, WAIT);
    expect(page.isConnected).toBe(true);
    expect(control.isConnected).toBe(true);
    expect(calls(fake, "GET", "/config")).toHaveLength(configReads);
    fireEvent.click(within(editor).getByRole("button", { name: "Close" }));
    await waitFor(() => expect(screen.queryByRole("dialog", { name: dialog })).toBeNull(), WAIT);
    expect(control.isConnected).toBe(true);
    expect(screen.getByRole("button", { name: trigger })).toBe(control);
  });

  it("closes a nested collection assistant with Escape while keeping the editor open", async () => {
    const { fake } = renderApp("/collections/new?draft=nested-usability");
    await screen.findByLabelText("Collection name", {}, WAIT);
    fireEvent.click(screen.getByRole("button", { name: "Describe it" }));
    await screen.findByRole("dialog", { name: "Describe a collection" });
    fireEvent.keyDown(document, { key: "Escape" });
    await waitFor(() =>
      expect(
        screen.queryByRole("dialog", { name: "Describe a collection" }),
      ).not.toBeInTheDocument(),
    );
    expect(screen.getByRole("region", { name: "Collection Studio" })).toBeInTheDocument();
    expect(screen.getByLabelText("Collection name")).toBeInTheDocument();
    expect(
      calls(fake, "POST", "/collections").filter((c) => c.path === "/collections"),
    ).toHaveLength(0);
  });

  it("shows newest logs first, including entries sharing a timestamp", async () => {
    const { fake } = renderApp("/maintenance/logs", {}, (f) => {
      f.state.logs = [
        { time: "2026-10-19T09:00:00Z", level: "info", message: "oldest" },
        { time: "2026-10-19T10:00:00Z", level: "info", message: "first at ten" },
        { time: "2026-10-19T10:00:00Z", level: "warn", message: "last at ten" },
      ];
    });
    const log = await screen.findByRole("log", { name: "Server logs" }, WAIT);
    await within(log).findByText("last at ten");
    expect(
      Array.from(log.querySelectorAll("time")).map((t) => t.parentElement!.textContent),
    ).toEqual([
      expect.stringContaining("last at ten"),
      expect.stringContaining("first at ten"),
      expect.stringContaining("oldest"),
    ]);
    fake.state.logs.push({ time: "2026-10-19T11:00:00Z", level: "info", message: "new arrival" });
    fireEvent.click(screen.getByRole("button", { name: "Reload" }));
    await within(log).findByText("new arrival");
    expect(log.querySelector("time")!.parentElement).toHaveTextContent("new arrival");
    expect(screen.getByLabelText("Live updates")).not.toBeChecked();
  });
});
