import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import type { StartupStatus } from "../lib/ops";
import {
  reconnect,
  reconnectInit,
  reconnectSteps,
  SLOW_AFTER_MS,
  stepState,
  type ReconnectState,
} from "../lib/reconnect";
import { calls, renderApp } from "./harness";

const OLD = "2026-10-31T08:00:00Z";
const NEW = "2026-10-31T10:15:03Z";

function status(
  phase: StartupStatus["phase"],
  started: string,
  extra: Partial<StartupStatus> = {},
): StartupStatus {
  return {
    phase,
    ready: phase === "ready",
    message: "",
    started,
    since: started,
    elapsedSeconds: 1,
    ...extra,
  };
}

function run(state: ReconnectState, ...events: Parameters<typeof reconnect>[1][]) {
  return events.reduce(reconnect, state);
}

describe("reconnect state machine", () => {
  it("follows a restart through stopping, the gap and the new start", () => {
    let s = reconnectInit(OLD, false, 0);
    expect(s.step).toBe("stopping");
    s = run(s, { type: "status", status: status("ready", OLD) });
    expect(s.step).toBe("stopping"); // the old server is about to stop
    s = run(s, { type: "status", status: status("stopping", OLD) }, { type: "unreachable" });
    expect(s.step).toBe("stopping");
    s = run(s, {
      type: "status",
      status: status("restoring", NEW, { restoring: "ether-1.tar.gz" }),
    });
    expect(s.step).toBe("restoring");
    expect(s.restarted).toBe(true);
    s = run(s, { type: "status", status: status("migrating", NEW, { step: 3, steps: 12 }) });
    expect(s.step).toBe("starting");
    expect(s.status?.step).toBe(3);
    // A blip after the new start reported in is still the new start.
    s = run(s, { type: "unreachable" });
    expect(s.step).toBe("starting");
    s = run(s, { type: "status", status: status("ready", NEW) });
    expect(s.step).toBe("done");
    expect(stepState("restoring", s)).toBe("complete");
  });

  it("knows the server came back even when the gap was never seen", () => {
    const s = run(reconnectInit(OLD, false, 0), { type: "status", status: status("ready", NEW) });
    expect(s.step).toBe("done");
  });

  it("waits for viewers before stopping", () => {
    let s = reconnectInit(OLD, true, 0);
    expect(s.step).toBe("waiting");
    s = run(s, { type: "status", status: status("ready", OLD) });
    expect(s.step).toBe("waiting");
    s = run(s, { type: "status", status: status("stopping", OLD) });
    expect(s.step).toBe("stopping");
    expect(stepState("waiting", s)).toBe("complete");
  });

  it("marks a failed start on the starting step", () => {
    const s = run(reconnectInit(OLD, false, 0), {
      type: "status",
      status: status("failed", NEW, { failure: { code: "migration_failed" } }),
    });
    expect(s.step).toBe("failed");
    expect(stepState("stopping", s)).toBe("complete");
    expect(stepState("starting", s)).toBe("failed");
    expect(stepState("done", s)).toBe("upcoming");
  });

  it("says when it is taking long, but never once done", () => {
    let s = run(reconnectInit(OLD, false, 0), { type: "tick", at: SLOW_AFTER_MS + 1 });
    expect(s.slow).toBe(true);
    s = run(
      s,
      { type: "status", status: status("ready", NEW) },
      { type: "tick", at: SLOW_AFTER_MS * 2 },
    );
    expect(s.slow).toBe(false);
  });

  it("follows a cold start from any status (no baseline)", () => {
    let s = reconnectInit(null, false, 0);
    expect(s.step).toBe("starting");
    expect(reconnectSteps("startup", s)).toEqual(["starting", "done"]);
    s = run(s, { type: "status", status: status("restoring", NEW) });
    expect(reconnectSteps("startup", s)).toEqual(["restoring", "starting", "done"]);
    s = run(s, { type: "status", status: status("ready", NEW) });
    expect(s.step).toBe("done");
  });

  it("resets when what it follows changes", () => {
    const s = run(
      reconnectInit(OLD, false, 0),
      { type: "status", status: status("ready", NEW) },
      { type: "reset", baseline: NEW, waitForIdle: true, at: 5 },
    );
    expect(s).toMatchObject({ step: "waiting", baseline: NEW, restarted: false, since: 5 });
  });
});

const LONG = { timeout: 15_000 };

describe("restart from the console", () => {
  it("shows who is watching, waits for them by default and follows the restart back", async () => {
    const { fake } = renderApp("/maintenance/backups", { opsPace: 20, viewers: 2 });
    // The first test here pays the lazy Maintenance page import, which can
    // take more than findBy's default second under a loaded full run.
    fireEvent.click(await screen.findByRole("button", { name: "Restart Ether" }, LONG));
    const drawer = await screen.findByRole("dialog");
    expect(await within(drawer).findByText("2 streams are playing")).toBeInTheDocument();
    expect(within(drawer).getByText("The Long Harbor (2021)")).toBeInTheDocument();
    expect(within(drawer).getByText(/systemd/)).toBeInTheDocument();
    const wait = within(drawer).getByRole("radio", { name: /Wait until nobody is watching/ });
    expect(wait).toBeChecked();
    // Restart now instead: the button says it interrupts.
    fireEvent.click(within(drawer).getByRole("radio", { name: /Restart now/ }));
    fireEvent.click(within(drawer).getByRole("button", { name: "Restart Ether" }));
    await waitFor(() => expect(calls(fake, "POST", "/server/restart").length).toBe(1));
    expect(calls(fake, "POST", "/server/restart")[0]!.body).toEqual({ whenIdle: false });
    expect(
      await within(drawer).findByRole("list", { name: "Restart progress" }),
    ).toBeInTheDocument();
    expect(await within(drawer).findByText("Back online", {}, LONG)).toBeInTheDocument();
    await waitFor(
      () => expect(within(drawer).getByRole("button", { name: "Continue" })).toBeEnabled(),
      LONG,
    );
  }, 20_000);

  it("can call off a restart that is waiting for viewers", async () => {
    const { fake } = renderApp("/maintenance/backups", { opsPace: 200, viewers: 2 });
    fireEvent.click(await screen.findByRole("button", { name: "Restart Ether" }));
    const drawer = await screen.findByRole("dialog");
    fireEvent.click(await within(drawer).findByRole("button", { name: "Restart when idle" }));
    expect(
      await within(drawer).findByText("Ether restarts as soon as nobody is watching."),
    ).toBeInTheDocument();
    fireEvent.click(within(drawer).getByRole("button", { name: "Cancel restart" }));
    await waitFor(() => expect(calls(fake, "DELETE", "/server/restart").length).toBe(1));
    expect(await within(drawer).findByText("What happens")).toBeInTheDocument();
  });
});

describe("guided restore", () => {
  it("checks a snapshot, says what happens, restores it and shows the report", async () => {
    const { fake } = renderApp("/maintenance/backups", { opsPace: 20 });
    const restore = await screen.findAllByRole("button", { name: /^Restore ether-/ });
    fireEvent.click(restore[0]!);
    const drawer = await screen.findByRole("dialog");
    expect(await within(drawer).findByText("What happens")).toBeInTheDocument();
    expect(within(drawer).getByText(/move to a rollback folder first/)).toBeInTheDocument();
    expect(within(drawer).getByText(/are replaced by the backup/)).toBeInTheDocument();
    expect(
      within(drawer).getByText("Nobody is watching right now, so no one is interrupted."),
    ).toBeInTheDocument();
    fireEvent.click(within(drawer).getByRole("button", { name: "Restore and restart" }));
    await waitFor(() =>
      expect(calls(fake, "POST", "/adoption/backups/restore/apply/").length).toBe(1),
    );
    const progress = await within(drawer).findByRole("list", { name: "Restore progress" });
    for (const step of ["Stopping", "Restoring", "Starting", "Restored"])
      expect(within(progress).getByText(step)).toBeInTheDocument();
    expect(await within(drawer).findByText("Backup restored", {}, LONG)).toBeInTheDocument();
    expect(within(drawer).getByText("Database checked — no problems found.")).toBeInTheDocument();
    expect(within(drawer).getByRole("button", { name: "Back to Ether" })).toBeEnabled();
  }, 20_000);

  it("keeps the password step on a wrong password and discards a cancelled check", async () => {
    const { fake } = renderApp("/maintenance/backups", { opsPace: 20 });
    const file = new File(["ETHERBK1-encrypted"], "ether-old.etherbackup");
    fireEvent.change(await screen.findByLabelText("Choose backup file"), {
      target: { files: [file] },
    });
    const drawer = await screen.findByRole("dialog");
    const pw = await within(drawer).findByLabelText("Backup password");
    fireEvent.change(pw, { target: { value: "wrong password" } });
    fireEvent.click(within(drawer).getByRole("button", { name: "Check backup" }));
    expect(await within(drawer).findByText(/wrong password/)).toBeInTheDocument();
    expect(within(drawer).getByLabelText("Backup password")).toBeInTheDocument();
    fireEvent.change(within(drawer).getByLabelText("Backup password"), {
      target: { value: "correct horse battery" },
    });
    fireEvent.click(within(drawer).getByRole("button", { name: "Check backup" }));
    expect(await within(drawer).findByText("ether-old.etherbackup")).toBeInTheDocument();
    fireEvent.click(within(drawer).getByRole("button", { name: "Cancel" }));
    await waitFor(() =>
      expect(calls(fake, "DELETE", "/adoption/backups/restore/staged/").length).toBe(1),
    );
  });

  it("guides a cluster node instead of restoring from one node", async () => {
    const { fake } = renderApp("/maintenance/backups", { cluster: true });
    const restore = await screen.findAllByRole("button", { name: /^Restore ether-/ });
    fireEvent.click(restore[0]!);
    const drawer = await screen.findByRole("dialog");
    expect(await within(drawer).findByText("This server is part of a cluster")).toBeInTheDocument();
    expect(
      within(drawer).getByText("Stop Ether on every node in the cluster."),
    ).toBeInTheDocument();
    expect(within(drawer).queryByRole("button", { name: "Restore and restart" })).toBeNull();
    expect(calls(fake, "POST", "/adoption/backups/restore/apply/")).toHaveLength(0);
  });
});

describe("startup page", () => {
  it("shows a database update with its progress, then opens the console", async () => {
    renderApp("/", { startup: "migrating" });
    expect(
      await screen.findByRole("heading", { name: "Updating the database" }),
    ).toBeInTheDocument();
    expect(await screen.findByText(/Applying update \d+ of 14/)).toBeInTheDocument();
    expect(screen.getByRole("list", { name: "Startup progress" })).toBeInTheDocument();
  });

  it("explains a failed start and restores a backup after the owner's password sign-in", async () => {
    const { fake } = renderApp("/", { startup: "failed:database_corrupt", opsPace: 20 });
    expect(
      await screen.findByRole("heading", { name: "Ether couldn't start" }),
    ).toBeInTheDocument();
    expect(await screen.findByText("The database is damaged")).toBeInTheDocument();
    // Every available sign-in is offered; the owner's password first.
    for (const tab of ["Password", "Plex", "Recovery code"])
      expect(await screen.findByRole("tab", { name: tab })).toBeInTheDocument();
    fireEvent.change(await screen.findByLabelText("Username"), { target: { value: "owner" } });
    fireEvent.change(screen.getByLabelText("Password"), { target: { value: "wrong" } });
    fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
    expect(
      await screen.findByText("That username and password don't match the owner's."),
    ).toBeInTheDocument();
    fireEvent.change(screen.getByLabelText("Password"), {
      target: { value: "correct horse battery" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
    const backups = await screen.findByRole("list", { name: "Backups on this server" }, LONG);
    fireEvent.click(
      within(backups).getAllByRole("button", { name: /Restore the backup from/ })[0]!,
    );
    const confirm = await screen.findByRole("dialog", { name: "Restore this backup?" });
    fireEvent.click(within(confirm).getByRole("button", { name: "Restore and start" }));
    await waitFor(() =>
      expect(calls(fake, "POST", "/startup/recovery/restore/snapshot/").length).toBe(1),
    );
    // Restored and started: the console takes over.
    expect(await screen.findByRole("link", { name: "Ether home" }, LONG)).toBeInTheDocument();
  }, 25_000);

  it("signs in with the one-time recovery code and offers to undo the last restore", async () => {
    const { fake } = renderApp("/", {
      startup: "failed:migration_failed",
      opsPace: 20,
      restore: "restored",
    });
    fireEvent.click(await screen.findByRole("tab", { name: "Recovery code" }));
    expect(screen.getByText("recovery-code.txt")).toBeInTheDocument();
    const input = screen.getByLabelText("Recovery code");
    fireEvent.change(input, { target: { value: "AAAA-BBBB-CCCC" } });
    fireEvent.click(screen.getByRole("button", { name: "Continue" }));
    expect(await screen.findByText(/That recovery code isn't right/)).toBeInTheDocument();
    fireEvent.change(input, { target: { value: "k7qf m2xd 9prt" } });
    fireEvent.click(screen.getByRole("button", { name: "Continue" }));
    fireEvent.click(await screen.findByRole("button", { name: "Undo restore" }, LONG));
    const confirm = await screen.findByRole("dialog", { name: "Undo the last restore?" });
    fireEvent.click(within(confirm).getByRole("button", { name: "Undo and start" }));
    await waitFor(() =>
      expect(calls(fake, "POST", "/startup/recovery/restore/undo").length).toBe(1),
    );
  }, 20_000);

  it("signs in with Plex", async () => {
    vi.stubGlobal(
      "open",
      vi.fn(() => null),
    );
    renderApp("/", { startup: "failed:database_corrupt" });
    fireEvent.click(await screen.findByRole("tab", { name: "Plex" }));
    fireEvent.click(await screen.findByRole("button", { name: "Sign in with Plex" }));
    expect(
      await screen.findByRole("list", { name: "Backups on this server" }, LONG),
    ).toBeInTheDocument();
  }, 20_000);
});

describe("restore report", () => {
  it("says when a restored backup couldn't start and the previous data came back", async () => {
    renderApp("/maintenance/backups", { restore: "rolledback" });
    expect(await screen.findByText("The backup couldn't start")).toBeInTheDocument();
    expect(
      screen.getByText("Ether put your previous data back automatically, so nothing was lost."),
    ).toBeInTheDocument();
    expect(screen.getByText(/no such column/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Undo restore" })).toBeNull();
  });
});
