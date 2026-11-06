import { act, cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { calls, renderApp } from "./harness";

afterEach(cleanup);

it("keeps Plex inside the shared grid and persists only the global flag", async () => {
  const { fake } = renderApp("/services/overview");
  const card = await screen.findByRole("article", { name: "Plex watch sync" });
  await waitFor(() => expect(within(card).getByRole("switch")).toBeEnabled());
  const grid = card.closest("ul")!;
  expect(grid).toBeTruthy();
  expect(within(grid).getAllByRole("article")).toHaveLength(3);
  expect(within(card).getByText("On")).toBeInTheDocument();
  fireEvent.click(within(card).getByRole("switch"));
  await waitFor(() => expect(fake.state.config.plexServers?.enabled).toBe(false));
  expect(calls(fake, "PUT", "/config/plexservers")[0]?.body).toEqual({ enabled: false });
  expect(within(card).getByText("Off")).toBeInTheDocument();
  fireEvent.click(within(card).getByRole("switch"));
  await waitFor(() => expect(fake.state.config.plexServers?.enabled).toBe(true));
  expect(within(card).getByRole("link", { name: "Manage Plex watch sync" })).toHaveAttribute(
    "href",
    "/services/plexservers",
  );
});

it.each([false, true])("shows setup and attention status (problem=%s)", async (problem) => {
  renderApp("/services/overview", {}, (f) => {
    (f.state as unknown as { plexServers: unknown[] }).plexServers = problem
      ? [{ id: 1, name: "PMS", problem: "token", viewers: [] }]
      : [];
  });
  const card = await screen.findByRole("article", { name: "Plex watch sync" });
  await waitFor(() => expect(within(card).getByRole("switch")).toBeEnabled());
  expect(within(card).getByText(problem ? "Needs attention" : "Not set up")).toBeInTheDocument();
  expect(
    within(card).getByRole("link", {
      name: problem ? "Manage Plex watch sync" : "Set up Plex watch sync",
    }),
  ).toBeInTheDocument();
});

it("holds sibling switches during a Plex save and restores state after failure", async () => {
  let release!: () => void;
  const { fake } = renderApp("/services/overview", {}, (f) => {
    const original = f.fetch;
    f.fetch = async (input, init) => {
      if (String(input).endsWith("/config/plexservers") && init?.method === "PUT") {
        await new Promise<void>((resolve) => {
          release = resolve;
        });
        return new Response(JSON.stringify({ error: "save_failed", message: "Failed" }), {
          status: 500,
        });
      }
      return original(input, init);
    };
  });
  const toggle = await screen.findByRole("switch", { name: "Plex watch sync enabled" });
  await waitFor(() => expect(toggle).toBeEnabled());
  fireEvent.click(toggle);
  await waitFor(() => expect(release).toBeTypeOf("function"));
  expect(toggle).toBeDisabled();
  expect(screen.getByRole("switch", { name: "Trakt enabled" })).toBeDisabled();
  expect(screen.getByRole("switch", { name: "Simkl enabled" })).toBeDisabled();
  await act(async () => release());
  await waitFor(() => expect(toggle).toBeEnabled());
  expect(toggle).toBeChecked();
  expect(fake.state.config.plexServers?.enabled).toBe(true);
});

it("opens one management drawer from the summary without a second Plex tab", async () => {
  renderApp("/services/overview");
  fireEvent.click(await screen.findByRole("link", { name: "Manage Plex watch sync" }));
  const manager = await screen.findByRole("dialog", {
    name: "Sync watch state with your Plex servers",
  });
  expect(screen.queryByRole("tab", { name: "Plex watch sync" })).toBeNull();
  expect(within(manager).queryByRole("switch", { name: /enabled/ })).toBeNull();
  const add = await within(manager).findByRole("button", { name: "Add server" });
  expect(screen.getAllByRole("button", { name: "Add server" })).toHaveLength(1);
  expect(
    within(manager).getByText(/other Plex Media Servers you run alongside Ether/),
  ).toBeInTheDocument();
  fireEvent.click(add);
  const editor = await screen.findByRole("dialog", { name: "Add server" });
  expect(within(editor).getByLabelText("Address")).toBeInTheDocument();
});

it("keeps the legacy management URL on the same drawer while sync is off", async () => {
  renderApp("/services/plexservers", {}, (f) => {
    f.state.config.plexServers = { enabled: false };
  });
  const manager = await screen.findByRole("dialog", {
    name: "Sync watch state with your Plex servers",
  });
  expect(await within(manager).findByRole("button", { name: "Add server" })).toBeEnabled();
  expect(within(manager).getByRole("button", { name: "Sync now" })).toBeDisabled();
});

it("refreshes the summary after removing a server in the one management drawer", async () => {
  const confirm = vi.spyOn(window, "confirm").mockReturnValue(true);
  renderApp("/services/overview");
  fireEvent.click(await screen.findByRole("link", { name: "Manage Plex watch sync" }));
  const manager = await screen.findByRole("dialog", {
    name: "Sync watch state with your Plex servers",
  });
  fireEvent.click(await within(manager).findByRole("button", { name: "Edit" }));
  const editor = await screen.findByRole("dialog", { name: "Edit Plex Media Server" });
  fireEvent.click(within(editor).getByRole("button", { name: "Remove server" }));
  await within(manager).findByText("No Plex Media Servers yet.");
  fireEvent.click(within(manager).getByRole("button", { name: "Close" }));
  expect(await screen.findByRole("link", { name: "Set up Plex watch sync" })).toBeInTheDocument();
  confirm.mockRestore();
});
