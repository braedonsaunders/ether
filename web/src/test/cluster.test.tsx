import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { destinations } from "../lib/sitemap";
import type { ClusterStatus } from "../lib/types";
import {
  CLUSTER_POLL_MS,
  clusterWarnings,
  tokenState,
  transcodeLabel,
  managerURL,
} from "../pages/settings/Cluster";
import { calls, renderApp } from "./harness";

const WAIT = { timeout: 4000 };

function rowOf(region: HTMLElement, text: string): HTMLElement {
  const row = within(region).getByText(text).closest("tr");
  if (!row) throw new Error(`no row for ${text}`);
  return row as HTMLElement;
}

describe("Server › Cluster", () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  it("is a sidebar page that search can find", async () => {
    expect(destinations().find((d) => d.href === "/cluster")).toMatchObject({ title: "Cluster" });
    renderApp("/cluster");
    expect(await screen.findByRole("heading", { name: "Cluster" }, WAIT)).toBeInTheDocument();
    expect(screen.getAllByRole("link", { name: "Cluster" }).length).toBeGreaterThan(0);
  });

  it("offers guided create and join on a standalone server", async () => {
    const { fake } = renderApp("/cluster");
    expect(await screen.findByText("Standalone (SQLite)", {}, WAIT)).toBeInTheDocument();
    expect(await screen.findByRole("button", { name: "Create cluster" })).toBeEnabled();
    expect(screen.getByRole("button", { name: "Join cluster" })).toBeEnabled();
    expect(screen.queryByText(/ether cluster init/)).toBeNull();
    expect(screen.getByRole("link", { name: "Read the cluster guide" })).toHaveAttribute(
      "href",
      expect.stringContaining("docs/install.md#running-a-cluster"),
    );
    expect(screen.queryByRole("button", { name: /Create join token/ })).toBeNull();
    expect(screen.queryByRole("region", { name: "Nodes" })).toBeNull();
    expect(calls(fake, "GET", "/cluster/settings")).toHaveLength(0);
  });

  it("tests a database, reviews the plan and automatically creates the cluster", async () => {
    const { fake } = renderApp("/cluster");
    fireEvent.click(await screen.findByRole("button", { name: "Create cluster" }, WAIT));
    const drawer = await screen.findByRole("dialog", { name: "Create cluster" });
    const next = within(drawer).getByRole("button", { name: "Test connection and continue" });
    expect(next).toBeDisabled();
    fireEvent.change(within(drawer).getByLabelText("Database host"), {
      target: { value: "db.lan" },
    });
    fireEvent.change(within(drawer).getByLabelText("Database password"), {
      target: { value: "secret@:/" },
    });
    fireEvent.click(next);
    await within(drawer).findByText("Connection verified", {}, WAIT);
    expect(calls(fake, "POST", "/cluster/setup/check")[0]!.body).toMatchObject({
      mode: "create",
      databaseUrl: "postgresql://ether:secret%40%3A%2F@db.lan:5432/ether?sslmode=verify-full",
    });
    expect(calls(fake, "POST", "/cluster/setup/apply")).toHaveLength(0);
    expect(within(drawer).getByText(/Create a portable backup/)).toBeInTheDocument();
    fireEvent.click(within(drawer).getByRole("button", { name: "Create cluster and restart" }));
    await screen.findByRole("region", { name: "Nodes" }, WAIT);
    expect(calls(fake, "POST", "/cluster/setup/apply")).toHaveLength(1);
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("warns when the owner explicitly weakens database transport protection", async () => {
    renderApp("/cluster");
    fireEvent.click(await screen.findByRole("button", { name: "Create cluster" }, WAIT));
    const drawer = await screen.findByRole("dialog", { name: "Create cluster" });
    const tls = within(drawer).getByLabelText("Database encryption");
    expect(tls).toHaveValue("verify-full");
    fireEvent.change(tls, { target: { value: "require" } });
    expect(within(drawer).getByRole("alert")).toHaveTextContent("does not verify");
    fireEvent.change(tls, { target: { value: "disable" } });
    expect(within(drawer).getByRole("alert")).toHaveTextContent("without encryption");
  });

  it("requires explicit replacement and allows recovery from an invalid join token", async () => {
    const { fake } = renderApp("/cluster");
    fireEvent.click(await screen.findByRole("button", { name: "Join cluster" }, WAIT));
    const drawer = await screen.findByRole("dialog", { name: "Join cluster" });
    fireEvent.change(within(drawer).getByLabelText("Join token"), { target: { value: "wrong" } });
    fireEvent.click(within(drawer).getByRole("button", { name: "Test connection and continue" }));
    await within(drawer).findByText(/Confirm replacement/, {}, WAIT);
    fireEvent.click(within(drawer).getByRole("checkbox"));
    fireEvent.click(within(drawer).getByRole("button", { name: "Test connection and continue" }));
    await within(drawer).findByText("The join token is not valid.", {}, WAIT);
    expect(calls(fake, "POST", "/cluster/setup/apply")).toHaveLength(0);
    fireEvent.change(within(drawer).getByLabelText("Join token"), {
      target: { value: "ether-join-v1:example" },
    });
    fireEvent.click(within(drawer).getByRole("button", { name: "Test connection and continue" }));
    await within(drawer).findByText("Connection verified", {}, WAIT);
    expect(within(drawer).getByText(/existing history is not merged/)).toBeInTheDocument();
    fireEvent.click(within(drawer).getByRole("button", { name: "Join cluster and restart" }));
    await screen.findByRole("region", { name: "Nodes" }, WAIT);
    expect(calls(fake, "POST", "/cluster/setup/apply")[0]!.body).toMatchObject({
      mode: "join",
      token: "ether-join-v1:example",
      replace: true,
    });
  });

  it("changes the local node weight and pauses and resumes new streams", async () => {
    const { fake } = renderApp("/cluster", { cluster: true });
    fireEvent.click(await screen.findByRole("button", { name: "Manage this node" }, WAIT));
    const drawer = await screen.findByRole("dialog", { name: "Manage this node" });
    fireEvent.change(within(drawer).getByLabelText("Node weight"), { target: { value: "3" } });
    fireEvent.click(within(drawer).getByRole("button", { name: "Save weight" }));
    await waitFor(() => expect(calls(fake, "PATCH", "/cluster/node")).toHaveLength(1));
    expect(calls(fake, "PATCH", "/cluster/node")[0]!.body).toEqual({ weight: 3 });
    fireEvent.click(await within(drawer).findByRole("button", { name: "Pause new streams" }));
    fireEvent.click(await within(drawer).findByRole("button", { name: "Resume new streams" }));
    await waitFor(() => expect(calls(fake, "PATCH", "/cluster/node")).toHaveLength(3));
    expect(calls(fake, "PATCH", "/cluster/node")[2]!.body).toEqual({ draining: false });
  });

  it("lists every node with its role, health, load and capabilities", async () => {
    renderApp("/cluster", { cluster: true });
    const nodes = await screen.findByRole("region", { name: "Nodes" }, WAIT);
    const den = rowOf(nodes, "den");
    expect(within(den).getByText("Leader")).toBeInTheDocument();
    expect(within(den).getByText("This node")).toBeInTheDocument();
    expect(within(den).getByText("Online")).toBeInTheDocument();
    expect(within(den).getByText("videotoolbox")).toBeInTheDocument();
    expect(within(den).getByLabelText("den weight")).toHaveTextContent("1");

    const attic = rowOf(nodes, "attic");
    expect(within(attic).queryByText("Leader")).toBeNull();
    expect(within(attic).getByLabelText("attic streams")).toHaveTextContent("2");
    expect(within(attic).getByLabelText("attic weight")).toHaveTextContent("2");
    expect(within(attic).getByText("198.51.100.20")).toBeInTheDocument();
    expect(within(attic).getByRole("link", { name: "Open manager for attic" })).toHaveAttribute(
      "href",
      "http://n-attic.test:32400/ether/cluster",
    );

    const garage = rowOf(nodes, "garage");
    expect(within(garage).getByText("Offline")).toBeInTheDocument();
    expect(within(garage).getByText(/Last seen 3 hours ago/)).toBeInTheDocument();
    expect(within(garage).getByText("Version 0.8.4")).toBeInTheDocument();
    expect(within(garage).getByText("Software")).toBeInTheDocument();

    // Different public addresses among live nodes are called out.
    expect(screen.getByText("Cluster needs attention")).toBeInTheDocument();
    expect(screen.getByText(/different public addresses/)).toBeInTheDocument();

    expect(screen.getByRole("heading", { name: "Shared settings" })).toBeInTheDocument();
    expect(screen.queryByText("v42")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "More options: Shared settings" }));
    expect(await screen.findByText("v42", {}, WAIT)).toBeInTheDocument();
    expect(screen.getByText("3f9c2a71d04be6a8")).toBeInTheDocument();
  });

  it("lists join tokens with their state", async () => {
    renderApp("/cluster", { cluster: true });
    const tokens = await screen.findByRole("region", { name: "Join tokens" }, WAIT);
    await within(tokens).findByText("t7k2", {}, WAIT);
    const used = rowOf(tokens, "t7k2");
    expect(await within(used).findByText("Used")).toBeInTheDocument();
    expect(within(used).getByText(/by attic/)).toBeInTheDocument();
    expect(within(used).queryByRole("button", { name: /Revoke/ })).toBeNull();
    const open = rowOf(tokens, "q9wd");
    expect(within(open).getByText("Unused")).toBeInTheDocument();
    expect(within(open).getByRole("button", { name: "Revoke token q9wd" })).toBeEnabled();
  });

  it("creates a join token in a drawer and shows it only once", async () => {
    const { fake } = renderApp("/cluster", { cluster: true });
    const tokens = await screen.findByRole("region", { name: "Join tokens" }, WAIT);
    fireEvent.click(within(tokens).getByRole("button", { name: /Create join token/ }));
    let drawer = await screen.findByRole("dialog", { name: "Create join token" });
    expect(within(drawer).queryByLabelText("Join token")).toBeNull();
    fireEvent.click(within(drawer).getByRole("radio", { name: "15 minutes" }));
    fireEvent.click(within(drawer).getByRole("button", { name: "Create join token" }));

    const field = await within(drawer).findByLabelText("Join token", {}, WAIT);
    const token = (field as HTMLTextAreaElement).value;
    expect(token).toMatch(/^ether-join-v1:/);
    expect(within(drawer).getByText("Keep this token secret")).toBeInTheDocument();
    expect(within(drawer).getByText(/database password/)).toBeInTheDocument();
    expect(within(drawer).getByText(/shown only once/)).toBeInTheDocument();
    expect(within(drawer).getByText(/choose Cluster → Join cluster/)).toBeInTheDocument();
    expect(within(drawer).getByRole("button", { name: /Copy token/ })).toBeInTheDocument();
    expect(within(drawer).queryByRole("button", { name: /Copy command/ })).toBeNull();
    expect(calls(fake, "POST", "/cluster/join-tokens")[0]!.body).toEqual({ ttlMinutes: 15 });

    // The new token is listed (by id only), and closing the drawer forgets the secret.
    const id = fake.state.joinTokens[0]!.id;
    expect(await within(tokens).findByText(id, {}, WAIT)).toBeInTheDocument();
    fireEvent.click(within(drawer).getByRole("button", { name: "Done" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(screen.queryByText(token)).toBeNull();
    fireEvent.click(within(tokens).getByRole("button", { name: /Create join token/ }));
    drawer = await screen.findByRole("dialog", { name: "Create join token" });
    expect(within(drawer).queryByLabelText("Join token")).toBeNull();
  });

  it("revokes an unused token after confirmation", async () => {
    const { fake } = renderApp("/cluster", { cluster: true });
    const tokens = await screen.findByRole("region", { name: "Join tokens" }, WAIT);
    fireEvent.click(await within(tokens).findByRole("button", { name: "Revoke token q9wd" }, WAIT));
    const confirm = await screen.findByRole("dialog", { name: "Revoke join token?" });
    fireEvent.click(within(confirm).getByRole("button", { name: "Revoke" }));
    await waitFor(() => expect(calls(fake, "DELETE", "/cluster/join-tokens/q9wd")).toHaveLength(1));
    await waitFor(() => expect(within(tokens).queryByText("q9wd")).toBeNull(), WAIT);
    expect(within(tokens).getByText("t7k2")).toBeInTheDocument();
  });

  it("refreshes membership every 10 seconds while open", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const { fake } = renderApp("/cluster", { cluster: true });
    await screen.findByRole("region", { name: "Nodes" }, WAIT);
    const before = calls(fake, "GET", "/cluster/").filter((c) => c.path === "/cluster/").length;
    await act(async () => {
      vi.advanceTimersByTime(CLUSTER_POLL_MS);
    });
    await waitFor(() =>
      expect(
        calls(fake, "GET", "/cluster/").filter((c) => c.path === "/cluster/").length,
      ).toBeGreaterThan(before),
    );
    expect(calls(fake, "GET", "/cluster/settings").length).toBeGreaterThan(1);
  });
});

describe("cluster helpers", () => {
  const now = new Date("2026-10-19T12:00:00Z");
  const base: ClusterStatus = {
    mode: "postgres",
    self: {
      id: "a",
      name: "a",
      version: "1",
      addrs: [],
      caps: { transcode: true },
      load: 0,
      startedAt: "",
      lastSeen: "",
      leader: true,
      live: true,
    },
    epoch: 1,
    nodes: [],
  };

  it("warns when this node loses the database, the event channel or clock sync", () => {
    const healthy = {
      channel: "c",
      busConnected: true,
      lastHeartbeat: "2026-10-19T11:59:58Z",
      clockSkewNs: 0,
    };
    expect(clusterWarnings({ ...base, health: healthy }, now)).toEqual([]);
    expect(
      clusterWarnings(
        { ...base, health: { ...healthy, lastHeartbeat: "2026-10-19T11:55:00Z" } },
        now,
      )[0],
    ).toMatch(/hasn't reached the database since 5 minutes ago/);
    expect(
      clusterWarnings({ ...base, health: { ...healthy, busConnected: false } }, now)[0],
    ).toMatch(/event connection/);
    expect(clusterWarnings({ ...base, health: { ...healthy, clockSkewNs: -5e9 } }, now)[0]).toMatch(
      /5 seconds off/,
    );
    expect(
      clusterWarnings({ ...base, error: "connection refused", warnings: ["nodes differ"] }, now),
    ).toEqual([
      "Nodes differ",
      "Couldn't read the cluster's nodes from the database: connection refused",
    ]);
  });

  it("builds safe links to peer managers", () => {
    expect(
      managerURL({ ...base.self, addrs: ["javascript:alert(1)", "http://node.lan:32400"] }),
    ).toBe("http://node.lan:32400/ether/cluster");
    expect(managerURL({ ...base.self, addrs: ["https://user:secret@node.lan"] })).toBeNull();
    expect(managerURL({ ...base.self, addrs: null })).toBeNull();
  });

  it("classifies tokens and transcoders", () => {
    const t = { id: "x", createdAt: "2026-10-19T11:00:00Z", expiresAt: "2026-10-19T12:30:00Z" };
    expect(tokenState(t, now)).toBe("unused");
    expect(tokenState({ ...t, expiresAt: "2026-10-19T11:30:00Z" }, now)).toBe("expired");
    expect(tokenState({ ...t, usedAt: "2026-10-19T11:10:00Z" }, now)).toBe("used");
    expect(transcodeLabel({ ...base.self, caps: { transcode: false } })).toBe("Unavailable");
    expect(transcodeLabel({ ...base.self, caps: { transcode: true, hwAccel: "none" } })).toBe(
      "Software",
    );
    expect(transcodeLabel({ ...base.self, caps: { transcode: true, hwAccel: "vaapi" } })).toBe(
      "vaapi",
    );
  });
});

describe("Cluster details elsewhere", () => {
  it("edits node weight on General only on a cluster node", async () => {
    const { fake } = renderApp("/server", { cluster: true });
    fireEvent.click(
      await screen.findByRole("button", { name: "More options: This computer" }, WAIT),
    );
    const weight = await screen.findByLabelText("Node weight", {}, WAIT);
    fireEvent.change(weight, { target: { value: "3" } });
    fireEvent.click(screen.getByRole("button", { name: /Save changes/ }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/server")).toHaveLength(1), WAIT);
    expect(calls(fake, "PUT", "/config/server")[0]!.body).toMatchObject({ clusterWeight: 3 });
  });

  it("hides node weight on a standalone server", async () => {
    renderApp("/server");
    await screen.findByLabelText("Server name", {}, WAIT);
    expect(screen.queryByLabelText("Node weight")).toBeNull();
  });

  it("shows which node holds each backup and saves a shared folder", async () => {
    const { fake } = renderApp("/maintenance/backups", { cluster: true });
    const snaps = await screen.findByRole("region", { name: "Snapshots" }, WAIT);
    expect(await within(snaps).findByText("den (this node)", {}, WAIT)).toBeInTheDocument();
    expect(within(snaps).getByText("attic")).toBeInTheDocument();
    const remote = fake.state.backups.find((b) => b.remote)!;
    expect(within(snaps).getByRole("button", { name: `Download ${remote.name}` })).toBeDisabled();
    expect(
      within(snaps).getByText("Stored on attic: download it from that node's admin page"),
    ).toBeVisible();
    const dir = await screen.findByLabelText("Shared backup folder", {}, WAIT);
    fireEvent.change(dir, { target: { value: "/mnt/backups/ether" } });
    fireEvent.click(screen.getByRole("button", { name: /Save changes/ }));
    await waitFor(() => expect(calls(fake, "POST", "/adoption/backups/settings")).toHaveLength(1));
    expect(calls(fake, "POST", "/adoption/backups/settings")[0]!.body).toEqual({
      intervalHours: 24,
      retain: 7,
      sharedDir: "/mnt/backups/ether",
    });
  });

  it("has no shared folder or location column on a standalone server", async () => {
    renderApp("/maintenance/backups");
    const snaps = await screen.findByRole("region", { name: "Snapshots" }, WAIT);
    await within(snaps).findByRole("button", { name: /^Download/ }, WAIT);
    expect(within(snaps).queryByText("Location")).toBeNull();
    expect(screen.queryByLabelText("Shared backup folder")).toBeNull();
  });

  it("shows this node's share of provider connections", async () => {
    renderApp("/sources/yours/usenet", { cluster: true });
    expect(
      await screen.findByText(/6 of 7 connections busy · this node: 7 of 20 connections/, {}, WAIT),
    ).toBeInTheDocument();
  });

  it("shows this node's share of debrid API connections", async () => {
    renderApp("/sources/yours/debrid", { cluster: true });
    const list = await screen.findByRole("region", { name: "Debrid providers" }, WAIT);
    expect(
      (await within(list).findAllByText(/this node: 2 of 6 API connections/, {}, WAIT)).length,
    ).toBeGreaterThan(0);
  });

  it("shows no node share on a standalone server", async () => {
    renderApp("/sources/yours/usenet");
    expect(await screen.findByText(/6 of 20 connections busy/, {}, WAIT)).toBeInTheDocument();
    expect(screen.queryByText(/this node:/)).toBeNull();
  });
});
