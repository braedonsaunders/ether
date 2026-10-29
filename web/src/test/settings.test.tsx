import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { calls, renderApp } from "./harness";
import { renderAppWith } from "./routes";

const WAIT = { timeout: 4000 };

describe("Settings", () => {
  it("shows and filters client families reported by other protocols", async () => {
    renderApp("/playback/devices");
    expect(await screen.findByText("Stremio TV", {}, WAIT)).toBeInTheDocument();
    fireEvent.change(screen.getByLabelText("Protocol"), { target: { value: "stremio" } });
    expect(screen.getByText("Stremio TV")).toBeInTheDocument();
    expect(screen.queryByText("Jellyfin TV")).toBeNull();
  });
  it("saves automatic franchise visibility without changing other catalog settings", async () => {
    const { fake } = renderApp("/catalog/settings");
    const toggle = await screen.findByLabelText("Hide automatic franchise collections", {}, WAIT);
    expect(toggle).not.toBeChecked();
    const before = { ...fake.state.config.catalog };
    fireEvent.click(toggle);
    fireEvent.click(screen.getByRole("button", { name: /Save changes/ }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/catalog")).toHaveLength(1), WAIT);
    expect(calls(fake, "PUT", "/config/catalog")[0]!.body).toEqual({
      ...before,
      hideFranchiseCollections: true,
    });
    await waitFor(() =>
      expect(screen.queryByRole("region", { name: "Unsaved changes" })).toBeNull(),
    );
    fireEvent.click(screen.getByLabelText("Hide automatic franchise collections"));
    fireEvent.click(screen.getByRole("button", { name: /Save changes/ }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/catalog")).toHaveLength(2), WAIT);
    expect(calls(fake, "PUT", "/config/catalog")[1]!.body).toEqual({
      ...before,
      hideFranchiseCollections: false,
    });
  });

  it("round-trips redacted secrets and saves only the changed service", async () => {
    const { fake } = renderApp("/services/opensubtitles", {}, (f) => {
      f.state.config.openSubtitles = {
        enabled: true,
        apiKey: "••••",
        username: "sam",
        password: "••••",
      };
    });
    expect(await screen.findByRole("heading", { name: "Integrations" }, WAIT)).toBeInTheDocument();
    fireEvent.change(await screen.findByLabelText("Username", {}, WAIT), {
      target: { value: "sam.rivera" },
    });
    fireEvent.click(screen.getByRole("button", { name: /Save changes/ }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/opensubtitles")).toHaveLength(1), WAIT);
    expect(calls(fake, "PUT", "/config/opensubtitles")[0]!.body).toEqual({
      enabled: true,
      apiKey: "••••",
      username: "sam.rivera",
      password: "••••",
    });
    expect(calls(fake, "PUT", "/config/trakt")).toHaveLength(0);
    expect(calls(fake, "PUT", "/config/llm")).toHaveLength(0);
    await waitFor(() =>
      expect(screen.queryByRole("region", { name: "Unsaved changes" })).toBeNull(),
    );
  });

  it("opens on an overview of every integration, essentials first", async () => {
    renderApp("/services");
    const essentials = await screen.findByRole("region", { name: "Essentials" }, WAIT);
    const optional = screen.getByRole("region", { name: "Optional" });
    expect(
      essentials.compareDocumentPosition(optional) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
    for (const [name, href] of [
      [/Apps/, "/apps/plex"],
      [/Catalog settings/, "/catalog/settings"],
      [/Sources › Debrid/, "/sources/debrid"],
    ] as const)
      expect(within(essentials).getByRole("link", { name })).toHaveAttribute("href", href);
    for (const [name, href] of [
      [/Set up Trakt/, "/services/trakt"],
      [/Manage language model/, "/services/llm"],
      [/Set up OpenSubtitles/, "/services/opensubtitles"],
      [/Metadata › Ratings/, "/metadata/ratings"],
      [/Playback › Markers/, "/playback/markers"],
      [/Sources › Indexers/, "/sources/indexers"],
    ] as const)
      expect(within(optional).getByRole("link", { name })).toHaveAttribute("href", href);
    expect(within(screen.getByRole("listitem", { name: "Language model" })).getByText("On"));
    // Credential forms live on their own tabs, not under the overview.
    expect(screen.queryByRole("button", { name: "Test Trakt" })).toBeNull();
  });

  it("gives each service its own tab with its connection test", async () => {
    renderApp("/services/trakt");
    expect(await screen.findByRole("button", { name: "Test Trakt" }, WAIT)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Test endpoint" })).toBeNull();
    fireEvent.click(screen.getByRole("tab", { name: "Language model" }));
    expect(await screen.findByRole("button", { name: "Test endpoint" }, WAIT)).toBeInTheDocument();
    expect(
      await screen.findByRole("heading", { name: "Language model features" }, WAIT),
    ).toBeVisible();
  });

  it("picks a language-model provider and a model from its live list", async () => {
    const { fake } = renderApp("/services/llm");
    fireEvent.click(await screen.findByRole("button", { name: "Provider" }, WAIT));
    fireEvent.click(await screen.findByRole("option", { name: /Anthropic — Claude/ }, WAIT));
    // Switching provider clears the old provider's model.
    expect(await screen.findByRole("button", { name: "Model" }, WAIT)).toHaveTextContent(
      "Choose a model",
    );
    fireEvent.change(screen.getByLabelText("API key"), { target: { value: "sk-ant-test" } });
    await waitFor(
      () =>
        expect(calls(fake, "POST", "/llm/models").at(-1)?.body).toMatchObject({
          provider: "anthropic",
          apiKey: "sk-ant-test",
        }),
      WAIT,
    );
    fireEvent.click(screen.getByRole("button", { name: "Model" }));
    fireEvent.click(await screen.findByRole("option", { name: /claude-opus-5-5/ }, WAIT));
    fireEvent.click(screen.getByRole("button", { name: /Save changes/ }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/llm")).toHaveLength(1), WAIT);
    expect(calls(fake, "PUT", "/config/llm")[0]!.body).toEqual({
      enabled: true,
      provider: "anthropic",
      baseUrl: "",
      apiKey: "sk-ant-test",
      model: "claude-opus-5-5",
    });
  });

  it("accepts a typed model ID and shows the provider's listing error", async () => {
    renderApp("/services/llm", {}, (f) => {
      f.state.config.llm = {
        enabled: true,
        provider: "openai",
        baseUrl: "",
        apiKey: "bad",
        model: "",
      };
    });
    fireEvent.click(await screen.findByRole("button", { name: "Model" }, WAIT));
    expect(
      await screen.findByText(/Couldn't load models: 401: invalid key/, {}, WAIT),
    ).toBeTruthy();
    fireEvent.change(screen.getByPlaceholderText("Search or type a model ID…"), {
      target: { value: "gpt-5-preview" },
    });
    fireEvent.click(await screen.findByRole("option", { name: /gpt-5-preview/ }, WAIT));
    expect(screen.getByRole("button", { name: "Model" })).toHaveTextContent("gpt-5-preview");
  });

  it("keeps unsaved edits across tabs and saves them together", async () => {
    const { fake } = renderApp("/services/trakt");
    fireEvent.change(await screen.findByLabelText("Client ID", {}, WAIT), {
      target: { value: "my-client" },
    });
    fireEvent.click(screen.getByRole("tab", { name: "OpenSubtitles" }));
    fireEvent.change(await screen.findByLabelText("Username", {}, WAIT), {
      target: { value: "sam" },
    });
    fireEvent.click(screen.getByRole("tab", { name: "Trakt" }));
    expect(await screen.findByLabelText("Client ID", {}, WAIT)).toHaveValue("my-client");
    fireEvent.click(screen.getByRole("button", { name: /Save changes/ }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/opensubtitles")).toHaveLength(1), WAIT);
    expect(calls(fake, "PUT", "/config/trakt")[0]!.body).toMatchObject({ clientId: "my-client" });
  });

  it("jumps to the tab holding a field the server rejected", async () => {
    renderAppWith("/services/trakt", (method, path) =>
      method === "PUT" && path === "/config/trakt"
        ? new Response(
            JSON.stringify({
              error: {
                code: "validation",
                message: "configuration is invalid",
                fields: [{ field: "trakt.clientId", message: "looks malformed" }],
              },
            }),
            { status: 400 },
          )
        : undefined,
    );
    fireEvent.change(await screen.findByLabelText("Client ID", {}, WAIT), {
      target: { value: "bad id" },
    });
    fireEvent.click(screen.getByRole("tab", { name: "Overview" }));
    await screen.findByRole("region", { name: "Essentials" }, WAIT);
    fireEvent.click(screen.getByRole("button", { name: /Save changes/ }));
    await waitFor(
      () =>
        expect(screen.getByRole("tab", { name: "Trakt" })).toHaveAttribute("aria-selected", "true"),
      WAIT,
    );
    expect(await screen.findByLabelText("Client ID", {}, WAIT)).toHaveValue("bad id");
    expect(screen.getAllByText("looks malformed").length).toBeGreaterThan(0);
  });

  it("shows server validation errors next to the field", async () => {
    const { fake } = renderApp("/server");
    const name = await screen.findByLabelText("Server name", {}, WAIT);
    expect(screen.queryByRole("button", { name: /^Edit / })).toBeNull();
    const original = fake.fetch;
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        if (String(input).endsWith("/config/server") && init?.method === "PUT") {
          return new Response(
            JSON.stringify({
              error: {
                code: "validation",
                message: "configuration is invalid",
                fields: [{ field: "server.friendlyName", message: "is required" }],
              },
            }),
            { status: 400 },
          );
        }
        return original(input, init);
      }),
    );
    fireEvent.change(name, { target: { value: "" } });
    fireEvent.click(screen.getByRole("button", { name: /Save changes/ }));
    expect(
      (await screen.findAllByText("Please fix the highlighted fields", {}, WAIT)).length,
    ).toBeGreaterThan(0);
    expect(screen.getAllByText("is required").length).toBeGreaterThan(0);
  });

  it("gives config sections the UI has no page for their own General tab, and saves them whole", async () => {
    const { fake } = renderApp("/server", {}, (f) => {
      f.state.config = {
        ...f.state.config,
        experiments: { enabled: true, explore: 0.2 },
        posters: {
          enabled: false,
          source: "postersplus",
          baseUrl: "",
          apiKey: "",
          urlTemplate: "",
        },
      } as never;
    });
    fireEvent.click(await screen.findByRole("tab", { name: "Experiments" }, WAIT));
    expect(screen.queryByRole("tab", { name: "Posters" })).not.toBeInTheDocument();
    expect(await screen.findByRole("heading", { name: "Experiments" }, WAIT)).toBeInTheDocument();
    fireEvent.change(await screen.findByLabelText("Explore", {}, WAIT), {
      target: { value: "0.4" },
    });
    fireEvent.click(screen.getByRole("button", { name: /Save changes/ }));
    await waitFor(() => expect(calls(fake, "PUT", "/config")).toHaveLength(1), WAIT);
    expect(
      (calls(fake, "PUT", "/config")[0]!.body as { experiments: unknown }).experiments,
    ).toEqual({ enabled: true, explore: 0.4 });
  });

  it("redirects the old General poster editor to Metadata artwork", async () => {
    renderApp("/server/posters");
    expect(await screen.findByRole("switch", { name: "Ether posters" }, WAIT)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Configure poster provider" })).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "General" })).not.toBeInTheDocument();
  });

  it("edits downloads, personalization and trailers on General tabs without losing other edits", async () => {
    const { fake } = renderApp("/server");
    fireEvent.change(await screen.findByLabelText("Server name", {}, WAIT), {
      target: { value: "Den Ether" },
    });
    fireEvent.click(screen.getByRole("tab", { name: "Downloads" }));
    fireEvent.click(await screen.findByLabelText("Friends can download", {}, WAIT));
    expect(screen.queryByLabelText("Server name")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: /Save changes/ }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/server")).toHaveLength(1), WAIT);
    const wholeSaves = () => calls(fake, "PUT", "/config").filter((c) => c.path === "/config");
    await waitFor(() => expect(wholeSaves()).toHaveLength(1), WAIT);
    const whole = wholeSaves()[0]!.body as {
      server: { friendlyName: string };
      downloads: { allowFriends: boolean };
    };
    expect(whole.downloads.allowFriends).toBe(true);
    // The whole-config save merges into the live config, so it can't undo the server save.
    expect(whole.server.friendlyName).toBe("Den Ether");
  });

  it("opens Plex app settings on status and connection checks", async () => {
    renderApp("/apps/plex");
    expect(await screen.findByRole("heading", { name: "Connection checks" }, WAIT)).toBeVisible();
    expect(screen.getByText("Plex account")).toBeInTheDocument();
    expect(screen.queryByLabelText("Allow remote access")).toBeNull();
    fireEvent.click(screen.getByRole("tab", { name: "Remote access" }));
    expect(await screen.findByLabelText("Allow remote access", {}, WAIT)).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Connection checks" })).toBeNull();
    expect(screen.queryByText("Owner token")).toBeNull();
  });

  it("explains that Plex account sign-in remains active when apps are off", async () => {
    renderApp("/apps/plex", {}, (f) => {
      f.state.config.frontends = { plex: { enabled: false } };
    });
    expect(
      await screen.findByText("Plex apps: off (account sign-in still active)", {}, WAIT),
    ).toBeVisible();
  });

  it("keeps an unsaved app setting while switching tabs", async () => {
    renderApp("/apps/jellyfin");
    fireEvent.change(await screen.findByLabelText("Server name", {}, WAIT), {
      target: { value: "Household Jellyfin" },
    });
    fireEvent.click(screen.getByRole("tab", { name: "Silo" }));
    await screen.findByRole("spinbutton", { name: "Port" }, WAIT);
    fireEvent.click(screen.getByRole("tab", { name: "Jellyfin" }));
    expect(await screen.findByRole("textbox", { name: "Server name" }, WAIT)).toHaveValue(
      "Household Jellyfin",
    );
    expect(screen.getByRole("region", { name: "Unsaved changes" })).toBeVisible();
  });

  it("lets the owner opt in to the Jellyfin public viewer picker", async () => {
    const { fake } = renderApp("/apps/jellyfin");
    const picker = await screen.findByLabelText(
      "Show local viewer names on the sign-in screen",
      {},
      WAIT,
    );
    expect(picker).not.toBeChecked();
    fireEvent.click(picker);
    fireEvent.click(screen.getByRole("button", { name: /Save changes/ }));
    await waitFor(
      () => expect(calls(fake, "PUT", "/frontends/jellyfin/settings")).toHaveLength(1),
      WAIT,
    );
    expect(calls(fake, "PUT", "/frontends/jellyfin/settings")[0]!.body).toMatchObject({
      publicViewerPicker: true,
    });
  });

  it("redirects old Connectivity links to the matching Apps tab", async () => {
    renderApp("/remote-access/secure");
    expect(await screen.findByRole("heading", { name: "Apps" }, WAIT)).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "Secure connections" })).toHaveAttribute(
      "aria-selected",
      "true",
    );
  });

  it("puts every server page in the main sidebar, with no separate settings menu", async () => {
    renderApp("/server");
    await screen.findByRole("heading", { name: "General" }, WAIT);
    const sidebar = (name: string) => screen.queryAllByRole("link", { name });
    for (const name of ["General", "Apps", "Integrations", "Notifications", "Maintenance"])
      expect(sidebar(name).length).toBeGreaterThan(0);
    for (const name of ["Backups & import", "Updates", "Troubleshooting", "Logs"])
      expect(sidebar(name)).toHaveLength(0);
    expect(sidebar("Settings")).toHaveLength(0);
    // Config keys owned by other pages never show up as extra sections here.
    for (const name of ["Usenet", "Indexers", "Cache", "Resolver", "Quality", "Catalog", "Markers"])
      expect(screen.queryByRole("heading", { name })).toBeNull();
  });

  it("labels local file playback as a troubleshooting tool", async () => {
    renderApp("/troubleshooting");
    expect(
      await screen.findByRole("heading", { name: "Test playback with a local file" }, WAIT),
    ).toBeInTheDocument();
    expect(screen.getByLabelText("Serve test files")).toBeInTheDocument();
  });
});
