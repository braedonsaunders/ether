import { fireEvent, screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { renderApp } from "./harness";

describe("health reporting", () => {
  it("leaves an installed certificate unverified without holding the server incomplete", async () => {
    renderApp("/");
    const trigger = await screen.findByRole("button", {
      name: "Server health: Operational",
    });
    const health = screen.getByText("Health").closest("section")!;
    expect(within(health).getByText("Not verified")).toBeInTheDocument();
    expect(within(health).getByText("plex.direct")).toBeInTheDocument();
    expect(within(health).getByText("Operational")).toBeInTheDocument();
    expect(screen.queryByText("Status incomplete")).toBeNull();
    expect(screen.queryByText("All healthy")).toBeNull();
    expect(screen.queryByText("Everything is healthy")).toBeNull();
    expect(screen.queryByText("All systems normal")).toBeNull();
    fireEvent.click(trigger);
    const popover = screen.getByText("Server health").parentElement!;
    const certificate = within(popover).getByText("plex.direct").closest("a")!;
    expect(within(certificate).getByText("Not verified")).toBeInTheDocument();
    expect(within(certificate).queryByText("Healthy")).toBeNull();
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
      await screen.findByRole("button", { name: "Server health: Operational" }),
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

  it("shows a published custom route without claiming it is reachable", async () => {
    renderApp("/", {}, (fake) => {
      const original = fake.fetch;
      fake.fetch = async (input, init) => {
        const response = await original(input, init);
        if (!String(input).endsWith("/dashboard")) return response;
        const body = await response.json();
        const uri = "https://plex.example.com:443";
        body.sections.remote.remoteAccess = false;
        body.sections.remote.publishedUris = [uri];
        body.sections.remote.connections = [{ uri, custom: true, protocol: "https" }];
        delete body.sections.remote.plexReachable;
        return new Response(JSON.stringify(body), { status: 200 });
      };
    });
    expect(
      await screen.findByRole("button", { name: "Server health: Operational" }),
    ).toBeInTheDocument();
    expect(screen.getByText("Custom connection published")).toBeInTheDocument();
    expect(screen.getByText("Not verified")).toBeInTheDocument();
    expect(screen.queryByText("Local network only")).toBeNull();
    expect(screen.queryByText("Reachable outside your network")).toBeNull();
  });

  it("keeps intentional LAN-only from reading as incomplete", async () => {
    renderApp("/", {}, (fake) => {
      const original = fake.fetch;
      fake.fetch = async (input, init) => {
        const response = await original(input, init);
        if (!String(input).endsWith("/dashboard")) return response;
        const body = await response.json();
        body.sections.remote.remoteAccess = false;
        body.sections.remote.publishedUris = ["http://192.168.1.20:32400"];
        body.sections.remote.connections = [
          { uri: "http://192.168.1.20:32400", local: true, protocol: "http" },
        ];
        return new Response(JSON.stringify(body), { status: 200 });
      };
    });
    expect(
      await screen.findByRole("button", { name: "Server health: Operational" }),
    ).toBeInTheDocument();
    expect(screen.getByText("Local network only")).toBeInTheDocument();
    expect(screen.queryByText("Status incomplete")).toBeNull();
  });

  it("stays incomplete when required status is unreported", async () => {
    renderApp("/", {}, (fake) => {
      const original = fake.fetch;
      fake.fetch = async (input, init) => {
        const response = await original(input, init);
        if (!String(input).endsWith("/dashboard")) return response;
        const body = await response.json();
        body.sections = {};
        return new Response(JSON.stringify(body), { status: 200 });
      };
    });
    expect(
      await screen.findByRole("button", { name: "Server health: Status incomplete" }),
    ).toBeInTheDocument();
    expect(screen.getAllByText("Not reported").length).toBeGreaterThan(0);
  });

  it("still reports a rejected sign-in as a problem", async () => {
    renderApp("/", {}, (fake) => {
      const original = fake.fetch;
      fake.fetch = async (input, init) => {
        const response = await original(input, init);
        if (!String(input).endsWith("/dashboard")) return response;
        const body = await response.json();
        body.sections.remote.tokenRejected = true;
        return new Response(JSON.stringify(body), { status: 200 });
      };
    });
    expect(
      await screen.findByRole("button", { name: "Server health: Problem" }),
    ).toBeInTheDocument();
    expect(screen.getByText("plex.tv rejects this server's sign-in")).toBeInTheDocument();
  });

  it("does not treat a published https name as a custom route without Connection.Custom", async () => {
    renderApp("/", {}, (fake) => {
      const original = fake.fetch;
      fake.fetch = async (input, init) => {
        const response = await original(input, init);
        if (!String(input).endsWith("/dashboard")) return response;
        const body = await response.json();
        body.sections.remote.remoteAccess = false;
        body.sections.remote.publishedUris = ["https://plex.example.com"];
        body.sections.remote.connections = [
          { uri: "https://plex.example.com", custom: false, protocol: "https" },
        ];
        return new Response(JSON.stringify(body), { status: 200 });
      };
    });
    expect(
      await screen.findByRole("button", { name: "Server health: Operational" }),
    ).toBeInTheDocument();
    expect(screen.getByText("Local network only")).toBeInTheDocument();
    expect(screen.queryByText("Custom connection published")).toBeNull();
  });

  it("keeps incomplete certificate dates in the overall status", async () => {
    renderApp("/", {}, (fake) => {
      const original = fake.fetch;
      fake.fetch = async (input, init) => {
        const response = await original(input, init);
        if (!String(input).endsWith("/dashboard")) return response;
        const body = await response.json();
        delete body.sections.remote.cert.notBefore;
        delete body.sections.remote.cert.notAfter;
        return new Response(JSON.stringify(body), { status: 200 });
      };
    });
    expect(
      await screen.findByRole("button", { name: "Server health: Status incomplete" }),
    ).toBeInTheDocument();
    expect(screen.getByText("Certificate dates are incomplete")).toBeInTheDocument();
    expect(screen.queryByText("Not verified")).toBeNull();
    expect(screen.queryByText("Operational")).toBeNull();
  });

  it("warns when the certificate is not valid yet", async () => {
    renderApp("/", {}, (fake) => {
      const original = fake.fetch;
      fake.fetch = async (input, init) => {
        const response = await original(input, init);
        if (!String(input).endsWith("/dashboard")) return response;
        const body = await response.json();
        body.sections.remote.cert.notBefore = "2999-01-01T00:00:00Z";
        return new Response(JSON.stringify(body), { status: 200 });
      };
    });
    expect(
      await screen.findByRole("button", { name: "Server health: Needs attention" }),
    ).toBeInTheDocument();
    expect(screen.getByText("Certificate is not valid yet")).toBeInTheDocument();
    expect(screen.queryByText("Not verified")).toBeNull();
  });
});
