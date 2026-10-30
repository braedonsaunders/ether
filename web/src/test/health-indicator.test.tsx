import { fireEvent, screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { renderApp } from "./harness";

describe("health reporting", () => {
  it("does not keep checking or claim all healthy after receiving an unverified certificate", async () => {
    renderApp("/");
    const trigger = await screen.findByRole("button", { name: "Server health: Status incomplete" });
    const health = screen.getByText("Health").closest("section")!;
    expect(within(health).getByText("Not verified")).toBeInTheDocument();
    expect(screen.queryByText("All healthy")).toBeNull();
    expect(screen.queryByText("Everything is healthy")).toBeNull();
    fireEvent.click(trigger);
    const popover = screen.getByText("Server health").parentElement!;
    const certificate = within(popover).getByText("plex.direct").closest("a")!;
    expect(within(certificate).getByText("Not verified")).toBeInTheDocument();
    expect(within(certificate).getByText(/^Expires /)).toBeInTheDocument();
    expect(
      within(certificate).getByText(
        "Expiry alone does not verify certificate trust or device association.",
      ),
    ).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Server health: Checking…" })).toBeNull();
  });

  it("shows Checking only until the initial dashboard response arrives", async () => {
    let release!: () => void;
    const pending = new Promise<void>((resolve) => {
      release = resolve;
    });
    renderApp("/", {}, (fake) => {
      const original = fake.fetch;
      fake.fetch = async (input, init) => {
        if (String(input).endsWith("/dashboard")) await pending;
        return original(input, init);
      };
    });
    try {
      expect(
        await screen.findByRole("button", { name: "Server health: Checking…" }),
      ).toBeInTheDocument();
    } finally {
      release();
    }
    expect(
      await screen.findByRole("button", { name: "Server health: Status incomplete" }),
    ).toBeInTheDocument();
  });

  it("reports a failed dashboard request instead of continuing to check", async () => {
    renderApp("/", {}, (fake) => {
      const original = fake.fetch;
      fake.fetch = async (input, init) =>
        String(input).endsWith("/dashboard")
          ? new Response(JSON.stringify({ error: { message: "offline" } }), { status: 503 })
          : original(input, init);
    });
    expect(
      await screen.findByRole("button", { name: "Server health: Problem" }),
    ).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Server health: Checking…" })).toBeNull();
  });

  it("still reports expired certificates as a problem", async () => {
    renderApp("/", {}, (fake) => {
      const original = fake.fetch;
      fake.fetch = async (input, init) => {
        const response = await original(input, init);
        if (!String(input).endsWith("/dashboard")) return response;
        const body = await response.json();
        body.sections.remote.cert.notAfter = "2020-01-01T00:00:00Z";
        return new Response(JSON.stringify(body), { status: 200 });
      };
    });
    expect(
      await screen.findByRole("button", { name: "Server health: Problem" }),
    ).toBeInTheDocument();
    expect(screen.getByText("Certificate expired")).toBeInTheDocument();
  });
});
