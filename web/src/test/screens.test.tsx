import { fireEvent, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { calls, renderApp } from "./harness";

const WAIT = { timeout: 4000 };

describe.each(["/", "/activity"])("Playback progress on %s", (path) => {
  it.each([undefined, 0, 55000])(
    "uses timeline position %s instead of byte delivery",
    async (position) => {
      renderApp(path, {}, (fake) => {
        const original = fake.fetch;
        fake.fetch = async (input, init) => {
          const res = await original(input, init);
          const dashboard = String(input).endsWith("/dashboard");
          const sessions = String(input).endsWith("/status/sessions");
          if (!dashboard && !sessions) return res;
          const body = await res.json();
          for (const s of dashboard ? body.sections.sessions : body.value) {
            s.viewOffset = position;
            s.Info.duration = 60e9;
            s.Info.size = 100;
            s.BytesServed = 33;
          }
          return new Response(JSON.stringify(body), { status: 200 });
        };
      });
      if (position === undefined) {
        expect(
          await screen.findByText("Waiting for playback position", {}, WAIT),
        ).toBeInTheDocument();
        expect(screen.queryByText(/% watched/)).not.toBeInTheDocument();
      } else {
        expect(
          await screen.findByText(`${Math.round((position / 60000) * 100)}% watched`, {}, WAIT),
        ).toBeInTheDocument();
      }
      expect(screen.queryByText(/of .* delivered/)).not.toBeInTheDocument();
    },
  );
});

describe("Overview", () => {
  it("shows now playing, health, activity and best rows", async () => {
    renderApp("/");
    expect(await screen.findByText(/Sam Rivera · Living Room/, {}, WAIT)).toBeInTheDocument();
    expect(screen.getByText("Direct play")).toBeInTheDocument();
    expect(screen.getByText(/% watched/)).toBeInTheDocument();
    const health = screen.getByText("Health").closest("section")!;
    for (const label of [
      "Usenet",
      "Indexers",
      "Cache",
      "Catalog",
      "Remote access",
      "plex.direct",
    ]) {
      expect(within(health).getByText(label)).toBeInTheDocument();
    }
    expect(
      await screen.findByRole("img", { name: /Plays per day over the last 30 days/ }, WAIT),
    ).toBeInTheDocument();
    expect(screen.getByText("Plays · 30 days")).toBeInTheDocument();
    expect(await screen.findByText("Best rows")).toBeInTheDocument();
    // The header health indicator summarises everything.
    expect(screen.getByRole("button", { name: /Server health/ })).toBeInTheDocument();
  });

  it("shows live server instruments: CPU, memory, network, GPU and Ether itself", async () => {
    const { fake } = renderApp("/");
    const server = (await screen.findByRole("heading", { name: "Server" }, WAIT)).closest(
      "section",
    )!;
    const cpu = await within(server).findByRole("group", { name: "CPU" }, WAIT);
    expect(within(cpu).getByText(/12 cores/)).toBeInTheDocument();
    expect(
      within(cpu).getByRole("img", { name: /CPU use over the last 5 minutes/ }),
    ).toBeInTheDocument();
    expect(
      within(within(server).getByRole("group", { name: "Memory" })).getByText(/of 24.0 GiB/),
    ).toBeInTheDocument();
    const net = within(server).getByRole("group", { name: "Network" });
    expect(within(net).getByLabelText("Download")).toBeInTheDocument();
    expect(within(net).getByLabelText("Upload")).toBeInTheDocument();
    expect(within(net).getAllByText(/Mbit\/s/).length).toBeGreaterThanOrEqual(2);
    const gpu = within(server).getByRole("group", { name: "GPU · RTX 4080" });
    expect(within(gpu).getByText("Encode")).toBeInTheDocument();
    expect(
      within(within(gpu).getByText("Encode").parentElement!).getByText("18%"),
    ).toBeInTheDocument();
    expect(within(server).getByText(/\d+% CPU · 412\.0 MiB/)).toBeInTheDocument();
    expect(within(server).getByText("videotoolbox")).toBeInTheDocument();
    expect(calls(fake, "GET", "/system/load").length).toBeGreaterThan(0);
  });

  it("says when this hardware reports no GPU", async () => {
    renderApp("/", {}, (fake) => {
      const original = fake.fetch;
      fake.fetch = async (input, init) => {
        const res = await original(input, init);
        if (!String(input).endsWith("/system/load")) return res;
        const body = await res.json();
        body.gpus = [];
        return new Response(JSON.stringify(body), { status: 200 });
      };
    });
    const gpu = await screen.findByRole("group", { name: "GPU" }, WAIT);
    expect(within(gpu).getByText("Not reported by this hardware")).toBeInTheDocument();
  });

  it("lists problems first, each linking to the screen that fixes it", async () => {
    renderApp("/", {}, (fake) => {
      const original = fake.fetch;
      fake.fetch = async (input, init) => {
        const res = await original(input, init);
        if (!String(input).endsWith("/dashboard")) return res;
        const body = await res.json();
        body.sections.providers = { error: "Every provider refused the login" };
        return new Response(JSON.stringify(body), { status: 200 });
      };
    });
    const attention = await screen.findByRole("region", { name: "Needs attention" }, WAIT);
    expect(within(attention).getByText("Every provider refused the login")).toBeInTheDocument();
    expect(within(attention).getByRole("link", { name: "Fix" })).toHaveAttribute(
      "href",
      "/sources/usenet",
    );
  });
});

describe("Updates settings", () => {
  it("shows update status and asks the server to install", async () => {
    const { fake } = renderApp("/maintenance/updates");
    expect(await screen.findByText(/Version 0\.3\.0/, {}, WAIT)).toBeInTheDocument();
    expect(screen.getByText(/03:00–06:00 local time/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Update now" }));
    expect(
      await screen.findByRole("button", { name: "Install now anyway" }, WAIT),
    ).toBeInTheDocument();
    expect(calls(fake, "POST", "/updates/apply").length).toBe(1);
  });

  it("keeps the channel up front and tucks the window, feed and wait behind more options", async () => {
    renderApp("/maintenance/updates");
    expect(await screen.findByLabelText("Channel", {}, WAIT)).toBeInTheDocument();
    expect(screen.getByLabelText("Install updates automatically")).toBeInTheDocument();
    expect(screen.queryByLabelText("Update feed")).toBeNull();
    expect(screen.queryByLabelText("Wait for playback to finish")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "More options: Release channel" }));
    expect(await screen.findByLabelText("Maintenance window starts", {}, WAIT)).toHaveValue(
      "03:00",
    );
    expect(screen.getByLabelText("Maintenance window ends")).toHaveValue("06:00");
    expect(screen.getByLabelText("Update feed")).toHaveValue("");
    expect(screen.getByLabelText("Wait for playback to finish")).toHaveValue("");
  });
});

describe("Getting started", () => {
  afterEach(() => localStorage.removeItem("ether.getting-started.dismissed"));

  it("lists the remaining fresh-install steps and hides when dismissed", async () => {
    localStorage.removeItem("ether.getting-started.dismissed");
    renderApp("/", {}, (fake) => {
      fake.state.config.debrid = { ...fake.state.config.debrid!, providers: [] };
      fake.state.config.usenet.providers = [];
      fake.state.config.quality.default = "1080p";
      fake.state.config.frontends = { jellyfin: { enabled: true } } as typeof fake.state.config.frontends;
      const original = fake.fetch;
      fake.fetch = async (input, init) => {
        const response = await original(input, init);
        const url = String(input);
        if (url.endsWith("/dashboard")) {
          const body = await response.json();
          body.sections.sessions = [];
          return new Response(JSON.stringify(body), { status: 200 });
        }
        if (url.endsWith("/viewers")) {
          const body = await response.json();
          const viewers = (body.viewers ?? body).filter(
            (viewer: { isOwner?: boolean }) => viewer.isOwner,
          );
          return new Response(JSON.stringify({ viewers }), { status: 200 });
        }
        return response;
      };
    });
    const steps = await screen.findByRole("region", { name: "Next steps" }, WAIT);
    expect(within(steps).getByRole("link", { name: "Add a source" })).toHaveAttribute("href", "/sources");
    expect(screen.getByRole("link", { name: "Connect a device" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Add viewers" })).toHaveAttribute("href", "/viewers");
    expect(
      screen.getByRole("link", { name: "Playback quality: 1080p at home · change" }),
    ).toHaveAttribute("href", "/releases/quality");
    expect(within(steps).queryByRole("link", { name: "Connect Plex" })).toBeNull(); // Plex is off
    fireEvent.click(within(steps).getByRole("button", { name: "Dismiss" }));
    expect(screen.queryByRole("region", { name: "Next steps" })).not.toBeInTheDocument();
  });

  it("hides once source, device, viewers, and quality are done", async () => {
    localStorage.removeItem("ether.getting-started.dismissed");
    renderApp("/", {}, (fake) => {
      fake.state.config.quality.default = "4K";
    });
    expect(await screen.findByTestId("getting-started-clear", {}, WAIT)).toBeInTheDocument();
    expect(screen.queryByRole("region", { name: "Next steps" })).not.toBeInTheDocument();
  });
});
