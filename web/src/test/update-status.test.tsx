import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { UpdatesStatus } from "../pages/settings/sections";
import type { UpdateStatus } from "../lib/types";

const base: UpdateStatus = {
  enabled: true,
  current: "0.2.32",
  capability: "self-apply",
  state: "idle",
  autoUpdate: false,
  requested: false,
  force: false,
  window: { start: "03:00", end: "05:00", open: false },
  activeSessions: 0,
  quietPeriodSeconds: 60,
  confirming: false,
};

async function show(patch: Partial<UpdateStatus> = {}) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => new Response(JSON.stringify({ ...base, ...patch }))),
  );
  render(<UpdatesStatus />);
  await screen.findByText("Version 0.2.32");
}

describe("update availability confidence", () => {
  it("does not claim to be current before the first check", async () => {
    await show();
    expect(screen.queryByText("Ether is up to date")).not.toBeInTheDocument();
    expect(screen.getAllByText("Not checked yet")).toHaveLength(2);
  });

  it("shows a failed feed check instead of claiming to be current", async () => {
    await show({ lastCheck: "2026-10-29T20:00:00Z", lastError: "check: certificate trust failed" });
    expect(screen.queryByText("Ether is up to date")).not.toBeInTheDocument();
    expect(screen.getAllByText("check: certificate trust failed")).toHaveLength(2);
  });

  it("reports current after a successful check with no newer release", async () => {
    await show({ lastCheck: "2026-10-29T20:00:00Z" });
    expect(screen.getByText("Ether is up to date")).toBeInTheDocument();
  });

  it("keeps a known release installable when a later check fails", async () => {
    await show({
      lastCheck: "2026-10-29T20:00:00Z",
      lastError: "check: unavailable",
      available: { version: "0.2.45", kind: "installer", staged: false },
    });
    expect(screen.getByText("Version 0.2.45 — Idle")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Update now" })).toBeEnabled();
    expect(screen.getByText("check: unavailable")).toBeInTheDocument();
  });

  it("preserves the disabled explanation", async () => {
    await show({ enabled: false, disabledReason: "No update feed configured" });
    expect(screen.getByText("No update feed configured")).toBeInTheDocument();
    expect(screen.queryByText("Ether is up to date")).not.toBeInTheDocument();
  });
  it("preserves the skipped rollback explanation", async () => {
    await show({ state: "rolled-back", failedVersion: "0.2.44" });
    expect(
      screen.getByText("Version 0.2.44 was rolled back and will be skipped"),
    ).toBeInTheDocument();
    expect(screen.queryByText("Ether is up to date")).not.toBeInTheDocument();
  });
});
