import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { describe, expect, it } from "vitest";
import type { EtherConfig, UsenetProvider } from "../lib/types";
import {
  choiceLabel,
  normalizeTraces,
  resolutionCeilings,
  TraceDetail,
} from "../pages/releases/History";
import { calls, renderApp } from "./harness";

const WAIT = { timeout: 4000 };
async function choose(label: string, option: string) {
  fireEvent.click(screen.getByRole("button", { name: label }));
  fireEvent.click(await screen.findByRole("option", { name: option }));
}

async function pick(scope: HTMLElement, label: string, ...options: string[]) {
  fireEvent.click(within(scope).getByRole("button", { name: label }));
  for (const option of options) {
    fireEvent.click(await screen.findByRole("option", { name: option, hidden: true }));
  }
}

describe("Sources", () => {
  it("enables bounded repair without changing provider credentials", async () => {
    const { fake } = renderApp("/sources/usenet");
    fireEvent.click(await screen.findByRole("button", { name: "Configure repair" }, WAIT));
    const enabled = await screen.findByRole("switch", { name: "Enable repair" });
    expect(enabled).not.toBeChecked();
    fireEvent.click(enabled);
    fireEvent.change(screen.getByLabelText("Extra download budget"), { target: { value: "256" } });
    fireEvent.click(screen.getByRole("button", { name: "Save repair settings" }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/usenet")).toHaveLength(1), WAIT);
    const body = calls(fake, "PUT", "/config/usenet")[0]!.body as EtherConfig["usenet"];
    expect(body.repair).toMatchObject({ enabled: true, maxDownloadBytes: 256 * 1024 * 1024 });
    expect(body.providers).toHaveLength(2);
    expect(body.providers?.[0]?.password).toBe("••••");
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  });

  it("discards unsaved provider edits when its drawer closes", async () => {
    const { fake } = renderApp("/sources/usenet");
    fireEvent.click(await screen.findByRole("button", { name: "Edit Primary news" }, WAIT));
    fireEvent.change(screen.getByLabelText("Host"), { target: { value: "draft.example" } });
    fireEvent.click(screen.getByRole("button", { name: "Close" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    fireEvent.click(screen.getByRole("button", { name: "Edit Primary news" }));
    expect(await screen.findByLabelText("Host")).toHaveValue("news.example.com");
    expect(calls(fake, "PUT", "/config/usenet")).toHaveLength(0);
  });

  it("saves one provider from its drawer and keeps redacted secrets", async () => {
    const { fake } = renderApp("/sources/usenet");
    fireEvent.click(await screen.findByRole("button", { name: "Edit Primary news" }, WAIT));
    fireEvent.change(screen.getByLabelText("Host"), { target: { value: "eu.news.example.com" } });
    fireEvent.click(screen.getByRole("button", { name: "Save provider" }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/usenet")).toHaveLength(1), WAIT);
    const body = calls(fake, "PUT", "/config/usenet")[0]!.body as { providers: UsenetProvider[] };
    expect(body.providers[0]).toMatchObject({
      host: "eu.news.example.com",
      password: "••••",
      priority: 0,
    });
    expect(body.providers[1]).toMatchObject({ password: "••••", priority: 1, backfill: true });
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  });

  it("persists provider removal from its drawer and retains remaining credentials", async () => {
    const { fake } = renderApp("/sources/usenet");
    fireEvent.click(await screen.findByRole("button", { name: "Edit Primary news" }, WAIT));
    fireEvent.click(screen.getByRole("button", { name: "Remove provider" }));
    fireEvent.click(await screen.findByRole("button", { name: "Remove" }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/usenet")).toHaveLength(1));
    expect(calls(fake, "PUT", "/config/usenet")[0]!.body).toMatchObject({
      providers: [{ id: "p2", priority: 0, password: "••••" }],
    });
    await waitFor(() =>
      expect(screen.queryByRole("button", { name: "Edit Primary news" })).not.toBeInTheDocument(),
    );
    expect(screen.getByRole("button", { name: "Edit Block account" })).toBeInTheDocument();
  });

  it("reorders providers and saves the new priorities", async () => {
    const { fake } = renderApp("/sources/usenet");
    fireEvent.click(await screen.findByRole("button", { name: "Move Block account up" }, WAIT));
    await waitFor(() => expect(calls(fake, "PUT", "/config/usenet")).toHaveLength(1));
    expect(calls(fake, "PUT", "/config/usenet")[0]!.body).toMatchObject({
      providers: [
        { id: "p2", priority: 0 },
        { id: "p1", priority: 1 },
      ],
    });
  });

  it("imports indexers from Prowlarr in a drawer", async () => {
    renderApp("/sources/indexers");
    expect(
      await screen.findByRole("button", { name: "Edit Indexer One" }, WAIT),
    ).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: /Import from Prowlarr/ }));
    const drawer = await screen.findByRole("dialog", { name: "Import from Prowlarr" });
    expect(within(drawer).getByLabelText("Prowlarr URL")).toBeInTheDocument();
    expect(within(drawer).getByRole("button", { name: "Preview indexers" })).toBeDisabled();
  });
});

describe("Playback devices", () => {
  it("filters app compatibility by protocol and device", async () => {
    renderApp("/playback/devices");
    expect(await screen.findByText("Plex Web", {}, WAIT)).toBeInTheDocument();
    expect(screen.getByText("Jellyfin TV")).toBeInTheDocument();
    expect(screen.getByText("Swiftfin")).toBeInTheDocument();
    expect(screen.getByText(/Seen on this server/)).toBeInTheDocument();
    expect(screen.getAllByText("Reports its own").length).toBeGreaterThan(0);
    expect(screen.getByText("Stream link")).toBeInTheDocument();
    expect(screen.queryByText(/tells Ether/)).not.toBeInTheDocument();
    expect(screen.queryByText(/plays that stream/)).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Protocol" }));
    fireEvent.click(
      within(await screen.findByRole("listbox", { hidden: true })).getByText("Jellyfin"),
    );
    expect(screen.queryByText("Plex Web")).not.toBeInTheDocument();
    expect(screen.getByText("Jellyfin TV")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Device" }));
    fireEvent.click(
      within(await screen.findByRole("listbox", { hidden: true })).getByText("Apple TV"),
    );
    expect(screen.queryByText("Jellyfin TV")).not.toBeInTheDocument();
    expect(screen.getByText("Swiftfin")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("tab", { name: "Devices" }));
    expect(await screen.findByText("Profile 5", {}, WAIT)).toBeInTheDocument();
    expect(screen.getByText("H.265/HEVC")).toBeInTheDocument();
    expect(screen.getByText("Dolby Vision")).toBeInTheDocument();
  });

  it("saves an app compatibility override from a drawer", async () => {
    const { fake } = renderApp("/playback/devices");
    fireEvent.click(await screen.findByRole("tab", { name: "Devices" }, WAIT));
    fireEvent.click(await screen.findByRole("button", { name: "Edit Apple TV" }, WAIT));
    const drawer = await screen.findByRole("dialog", { name: "Edit Apple TV" });
    expect(within(drawer).getByText("Built in: Profile 5, Profile 8")).toBeInTheDocument();
    expect(within(drawer).getAllByText("Built in: 4K").length).toBeGreaterThan(0);
    await pick(drawer, "Dolby Vision profiles", "Profile 5", "Profile 7", "Profile 8");
    fireEvent.click(within(drawer).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/clients")).toHaveLength(1));
    expect(calls(fake, "PUT", "/config/clients")[0]!.body).toMatchObject({
      overrides: [{ id: "apple-tv", dolbyVisionProfiles: [5, 7, 8] }],
    });
  });

  it("adds an app override from a typed match", async () => {
    const { fake } = renderApp("/playback/devices");
    fireEvent.click(await screen.findByRole("button", { name: "Add" }, WAIT));
    const drawer = await screen.findByRole("dialog", { name: "Add an app" });
    fireEvent.change(within(drawer).getByLabelText("Match"), { target: { value: "Infuse" } });
    await pick(drawer, "Video codecs", "H.265/HEVC");
    fireEvent.click(within(drawer).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/clients")).toHaveLength(1));
    expect(calls(fake, "PUT", "/config/clients")[0]!.body).toMatchObject({
      overrides: [{ product: "Infuse", videoCodecs: ["hevc"] }],
    });
  });
});

describe("Releases", () => {
  it("edits client rules in a drawer using a device and named viewer", async () => {
    const { fake } = renderApp("/releases/devices");
    expect(await screen.findByRole("button", { name: "Edit rule 1" }, WAIT)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Device" })).not.toBeInTheDocument();
    expect(
      screen.queryByRole("switch", { name: "Convert Dolby and DTS audio to AAC" }),
    ).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Edit rule 1" }));
    const drawer = await screen.findByRole("dialog", { name: "Rule 1" });
    expect(within(drawer).getByRole("button", { name: "Device" })).toBeInTheDocument();
    const surroundSwitch = within(drawer).getByRole("switch", {
      name: "Convert Dolby and DTS audio to AAC",
    });
    expect(surroundSwitch).not.toBeChecked();
    expect(
      within(drawer).getByText(
        "For free Infuse and other players that can't play surround formats.",
      ),
    ).toBeInTheDocument();
    fireEvent.click(surroundSwitch);
    await choose("Protocol", "Plex");
    await choose("Device", "Plex Web");
    await choose("Viewer", "The Kids");
    await choose("Profile", "1080p");
    await choose("Maximum resolution", "1080p");
    fireEvent.click(screen.getByRole("button", { name: "Save rule" }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/quality")).toHaveLength(1));
    expect(calls(fake, "PUT", "/config/quality")[0]!.body).toMatchObject({
      rules: [
        {
          protocol: "plex",
          product: "Plex Web",
          userId: 3,
          profile: "1080p",
          maxHeight: 1080,
          convertSurroundAudio: true,
        },
      ],
    });
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    fireEvent.click(screen.getByRole("button", { name: "Edit rule 1" }));
    const reopened = await screen.findByRole("dialog", { name: "Rule 1" });
    expect(
      within(reopened).getByRole("switch", { name: "Convert Dolby and DTS audio to AAC" }),
    ).toBeChecked();
  });

  it("shows Never play filters and What matters most ladder in the profile drawer", async () => {
    renderApp("/releases/profiles");
    fireEvent.click(await screen.findByRole("button", { name: "Edit 4K" }, WAIT));
    const drawer = await screen.findByRole("dialog", { name: "4K" });
    expect(within(drawer).getByRole("group", { name: "Movies" })).toBeInTheDocument();
    expect(within(drawer).getByRole("group", { name: "TV episodes" })).toBeInTheDocument();
    expect(within(drawer).getByLabelText("Maximum MB per minute")).toHaveValue(1000);
    expect(within(drawer).getByLabelText("Episode maximum MB per minute")).toHaveValue(0);
    expect(within(drawer).getByRole("switch", { name: "Larger file" })).not.toBeChecked();
    expect(within(drawer).queryByText("Imported from TRaSH / Radarr / Sonarr")).toBeNull();
  });

  it("reorders steps in What matters most and saves the new ranking ladder", async () => {
    const { fake } = renderApp("/releases/profiles");
    fireEvent.click(await screen.findByRole("button", { name: "Edit 4K" }, WAIT));
    const drawer = await screen.findByRole("dialog", { name: "4K" });
    fireEvent.click(within(drawer).getByRole("button", { name: "Move Release type up" }));
    fireEvent.click(within(drawer).getByRole("button", { name: "Save profile" }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/quality")).toHaveLength(1));
    const body = calls(fake, "PUT", "/config/quality")[0]!.body as EtherConfig["quality"];
    const saved4k = body.profiles?.find((p) => p.name === "4K");
    expect(saved4k?.ranking).toEqual([
      "language",
      "connection",
      "releaseType",
      "resolution",
      "hdr",
      "verified",
      "videoCodec",
      "audioFormat",
      "size",
    ]);
  });

  it("shows Default at home and Default away badges and updates them from the profile drawer", async () => {
    const { fake } = renderApp("/releases/profiles");
    expect(await screen.findByText("Default at home", {}, WAIT)).toBeInTheDocument();
    expect(screen.getByText("Default away")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Edit 4K" }));
    const drawer = await screen.findByRole("dialog", { name: "4K" });
    const defaultsGroup = within(drawer).getByRole("group", { name: "Default for" });
    fireEvent.click(within(defaultsGroup).getByRole("button", { name: "At home" }));
    fireEvent.click(within(drawer).getByRole("button", { name: "Save profile" }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/quality")).toHaveLength(1));
    expect(calls(fake, "PUT", "/config/quality")[0]!.body).toMatchObject({
      default: "4K",
      remote: "1080p",
    });
  });

  it("renames a profile in its drawer and keeps rules pointing at it", async () => {
    const { fake } = renderApp("/releases/profiles");
    fireEvent.click(await screen.findByRole("button", { name: "Edit 4K" }, WAIT));
    const drawer = await screen.findByRole("dialog", { name: "4K" });
    fireEvent.change(within(drawer).getByLabelText("Name"), { target: { value: "UHD" } });
    fireEvent.click(within(drawer).getByRole("button", { name: "Save profile" }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/quality")).toHaveLength(1));
    const body = calls(fake, "PUT", "/config/quality")[0]!.body as EtherConfig["quality"];
    expect(body.profiles?.[0]?.name).toBe("UHD");
    expect(body.rules?.[0]).toMatchObject({ product: "Plex for Apple TV", profile: "UHD" });
  });

  it("removes a profile a client rule uses, with its rules (#9)", async () => {
    const { fake } = renderApp("/releases/profiles");
    fireEvent.click(await screen.findByRole("button", { name: "Edit 4K" }, WAIT));
    const drawer = await screen.findByRole("dialog", { name: "4K" });
    fireEvent.click(within(drawer).getByRole("button", { name: "Remove profile" }));
    expect(await screen.findByText(/The device rule using it will be removed/)).toBeInTheDocument();
    fireEvent.click(await screen.findByRole("button", { name: "Remove" }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/quality")).toHaveLength(1));
    const body = calls(fake, "PUT", "/config/quality")[0]!.body as EtherConfig["quality"];
    expect(body.profiles?.map((p) => p.name)).toEqual(["1080p"]);
    expect(body.rules).toEqual([]);
    expect(body).toMatchObject({ default: "1080p", remote: "1080p" });
  });

  it("rejects invalid custom-format JSON inside an imported profile", async () => {
    const { fake } = renderApp("/releases/profiles", {}, (f) => {
      const p = f.state.config.quality.profiles?.find((x) => x.name === "1080p");
      if (p) p.minCustomFormatScore = 0;
    });
    fireEvent.click(await screen.findByRole("button", { name: "Edit 1080p" }, WAIT));
    const drawer = await screen.findByRole("dialog", { name: "1080p" });
    fireEvent.click(within(drawer).getByRole("button", { name: "More options: Custom scoring" }));
    const defs = await within(drawer).findByLabelText("Custom format definitions");
    fireEvent.change(defs, { target: { value: "unfinished JSON" } });
    fireEvent.click(within(drawer).getByRole("button", { name: "Save profile" }));
    expect(await screen.findByText(/aren't valid JSON/)).toBeInTheDocument();
    expect(calls(fake, "PUT", "/config/quality")).toHaveLength(0);
    expect(calls(fake, "PUT", "/config/resolver")).toHaveLength(0);
  });

  it("saves score overrides on an imported profile", async () => {
    const { fake } = renderApp("/releases/profiles", {}, (f) => {
      const p = f.state.config.quality.profiles?.find((x) => x.name === "1080p");
      if (p) p.minCustomFormatScore = 0;
    });
    fireEvent.click(await screen.findByRole("button", { name: "Edit 1080p" }, WAIT));
    const drawer = await screen.findByRole("dialog", { name: "1080p" });
    fireEvent.click(within(drawer).getByRole("button", { name: "More options: Custom scoring" }));
    fireEvent.change(within(drawer).getByLabelText("Score overrides for this profile"), {
      target: { value: '{ "x265-hd": -500 }' },
    });
    fireEvent.click(within(drawer).getByRole("button", { name: "Save profile" }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/quality")).toHaveLength(1));
    const body = calls(fake, "PUT", "/config/quality")[0]!.body as EtherConfig["quality"];
    expect(body.profiles?.find((p) => p.name === "1080p")?.customFormatScores).toEqual({
      "x265-hd": -500,
    });
  });
});

describe("Source routing", () => {
  it("shows Tie-breaker with measured startup inside the profile drawer and saves which source leads", async () => {
    const { fake } = renderApp("/releases/profiles");
    fireEvent.click(await screen.findByRole("button", { name: "Edit 1080p" }, WAIT));
    const drawer = await screen.findByRole("dialog", { name: "1080p" }, WAIT);
    expect(
      await within(drawer).findByText(/debrid 410 ms \(22 plays\)/, {}, WAIT),
    ).toBeInTheDocument();
    expect(within(drawer).getByText(/Usenet 2\.1 s \(37 plays\)/)).toBeInTheDocument();
    expect(within(drawer).getByText("Applies to all profiles.")).toBeInTheDocument();
    fireEvent.click(within(drawer).getByRole("button", { name: "When two releases are equal" }));
    fireEvent.click(await screen.findByRole("option", { name: "Usenet first" }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/resolver")).toHaveLength(1));
    expect(calls(fake, "PUT", "/config/resolver")[0]!.body).toMatchObject({
      sourcePreference: "usenet",
    });
  });
});

describe("Release selection", () => {
  it("lets the owner set how many same-quality versions apps list on the Searching tab", async () => {
    const { fake } = renderApp("/releases/searching");
    const field = await screen.findByLabelText("Same-quality versions to offer", {}, WAIT);
    expect(field).toHaveValue(1);
    expect(screen.getByLabelText("Offer higher versions")).not.toBeChecked();
    fireEvent.change(field, { target: { value: "3" } });
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/resolver")).toHaveLength(1));
    expect(calls(fake, "PUT", "/config/resolver")[0]!.body).toMatchObject({
      sameQualityAlternatives: 3,
    });
  });

  it("shows How Ether searches as a clean section on the Searching tab", async () => {
    renderApp("/releases/searching");
    expect(await screen.findByLabelText("Releases to keep ready", {}, WAIT)).toHaveValue(3);
    expect(screen.getByLabelText("Health check samples")).toHaveValue(12);
    expect(screen.getByLabelText("Remember searches for")).toHaveValue("6h0m0s");
    expect(screen.getByLabelText("Retry unavailable titles after")).toHaveValue("24h0m0s");
  });

  it("edits blocked words next to blocked releases", async () => {
    renderApp("/releases/blocklist");
    expect(await screen.findByLabelText("Blocked release groups", {}, WAIT)).toBeInTheDocument();
    expect(screen.getByLabelText("Blocked terms")).toBeInTheDocument();
  });

  it("starts playback sooner from the Playback page", async () => {
    const { fake } = renderApp("/playback/transcoding");
    fireEvent.click(await screen.findByRole("button", { name: "More options: Start speed" }, WAIT));
    fireEvent.click(
      await screen.findByRole("switch", {
        name: "Start the top release before its health check completes",
      }),
    );
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/resolver")).toHaveLength(1));
    expect(calls(fake, "PUT", "/config/resolver")[0]!.body).toMatchObject({
      optimisticStart: true,
    });
  });
});

describe("Release traces", () => {
  it("shows discovery and limits, preserving unknown for older history", () => {
    const [oldTrace, newTrace] = normalizeTraces([
      { titleName: "Old" },
      {
        titleName: "New",
        discovery: [
          { path: "live-early", count: 2 },
          { path: "cache", count: 1, rescue: true },
          { path: "feed", count: 0 },
          { path: "live-complete", count: 3 },
        ],
        limits: {
          profileMinHeight: 720,
          profileMaxHeight: 1080,
          clientMaxHeight: 2160,
          clientMaxBitrateKbps: 5000,
        },
      },
    ]);
    const view = render(<TraceDetail trace={oldTrace!} />);
    expect(screen.getByText("Discovery: unknown")).toBeInTheDocument();
    expect(screen.getByText("Effective playback limits: unknown")).toBeInTheDocument();
    view.rerender(<TraceDetail trace={newTrace!} />);
    expect(
      screen.getByText(
        /Live search ended early \(2 releases\).*Rescue: Cached search \(1 release\).*Feed results \(0 releases\).*Live search completed \(3 releases\)/,
      ),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/Profile: 720p–1080p.*Height: 2160p.*Bitrate: 5000 kbps/),
    ).toBeInTheDocument();
    for (const [min, max, label] of [
      [0, 0, "Any resolution"],
      [1080, 0, "At least 1080p"],
      [0, 2160, "Up to 2160p"],
    ] as const) {
      view.rerender(
        <TraceDetail
          trace={{
            ...newTrace!,
            limits: {
              profileMinHeight: min,
              profileMaxHeight: max,
              clientMaxHeight: 0,
              clientMaxBitrateKbps: 0,
            },
          }}
        />,
      );
      expect(
        screen.getByText(
          `Effective playback limits: Profile: ${label} · Height: unlimited · Bitrate: unlimited`,
        ),
      ).toBeInTheDocument();
    }
  });
  it("normalizes typed and loosely shaped traces", () => {
    const t = normalizeTraces([
      {
        titleName: "Dune",
        candidates: [{ name: "Dune.1080p", score: 5, rejected: true, reason: "too big" }],
        err: "no luck",
        pinStatus: "unhealthy: health: unhealthy",
      },
      "junk",
    ]);
    expect(t).toHaveLength(1);
    expect(t[0]).toMatchObject({
      titleName: "Dune",
      err: "no luck",
      pinStatus: "unhealthy: health: unhealthy",
    });
    expect(t[0]!.candidates![0]).toMatchObject({
      name: "Dune.1080p",
      rejected: true,
      reason: "too big",
    });
  });

  it("maps max-height ceilings from the trace and from its history record", () => {
    const t = normalizeTraces([
      {
        titleName: "Dune",
        profileMaxHeight: 2160,
        clientMaxHeight: 1080,
        effectiveMaxHeight: 1080,
        candidates: [],
      },
      // As a history entry delivers them: on the record's nested trace.
      { titleName: "Old", trace: { profileMaxHeight: 1080, effectiveMaxHeight: 1080 } },
      { titleName: "Legacy" },
      // The real pre-PR shape: limits without profileConfiguredMaxHeight.
      {
        titleName: "LegacyLimits",
        limits: {
          profileMinHeight: 1080,
          profileMaxHeight: 1080,
          clientMaxHeight: 1080,
          clientMaxBitrateKbps: 0,
        },
      },
      // A new explicitly unlimited profile: configured recorded as 0.
      {
        titleName: "AnyProfile",
        limits: {
          profileMinHeight: 0,
          profileMaxHeight: 1080,
          profileConfiguredMaxHeight: 0,
          clientMaxHeight: 1080,
          clientMaxBitrateKbps: 0,
        },
      },
    ]);
    expect(t[0]).toMatchObject({
      profileMaxHeight: 2160,
      clientMaxHeight: 1080,
      effectiveMaxHeight: 1080,
    });
    expect(t[1]).toMatchObject({
      profileMaxHeight: 1080,
      clientMaxHeight: 0,
      effectiveMaxHeight: 1080,
    });
    expect(t[2]!.profileMaxHeight).toBeUndefined();
    expect(t[2]).toMatchObject({ clientMaxHeight: 0, effectiveMaxHeight: 0 });
    expect(resolutionCeilings(t[0]!)).toBe("Profile ≤2160 · Client ≤1080 · Effective ≤1080");
    expect(resolutionCeilings(t[1]!)).toBe("Profile ≤1080 · Client Any · Effective ≤1080");
    expect(resolutionCeilings(t[2]!)).toBeNull();
    // Legacy limits never recorded the configured ceiling: unknown,
    // so no ceilings line at all (not an invented "Profile Any").
    expect(t[3]!.profileMaxHeight).toBeUndefined();
    expect(t[3]).toMatchObject({ clientMaxHeight: 1080, effectiveMaxHeight: 1080 });
    expect(resolutionCeilings(t[3]!)).toBeNull();
    // Explicitly unlimited is present 0 and does render as "Any".
    expect(t[4]!.profileMaxHeight).toBe(0);
    expect(resolutionCeilings(t[4]!)).toBe("Profile Any · Client ≤1080 · Effective ≤1080");
    const chosen = normalizeTraces([
      { titleName: "Picked", chooser: "auto" },
      { titleName: "Version", chooser: "viewer", chosenVersion: "Movie.2160p" },
      { titleName: "Legacy" },
    ]);
    expect(choiceLabel(chosen[0]!.chooser, chosen[0]!.chosenVersion)).toBe("Auto");
    expect(choiceLabel(chosen[1]!.chooser, chosen[1]!.chosenVersion)).toBe(
      "Viewer/client picked version Movie.2160p",
    );
    expect(choiceLabel(chosen[2]!.chooser, chosen[2]!.chosenVersion)).toBeNull();
  });

  it("shows the ceilings in the trace detail", () => {
    render(
      <MemoryRouter>
        <TraceDetail
          trace={{
            titleKey: "603",
            titleName: "Dune",
            profile: "4K",
            profileMaxHeight: 2160,
            clientMaxHeight: 1080,
            effectiveMaxHeight: 1080,
            at: "",
            candidates: [],
            searchDuration: 0,
          }}
        />
      </MemoryRouter>,
    );
    expect(screen.getByText("Profile ≤2160 · Client ≤1080 · Effective ≤1080")).toBeInTheDocument();
  });

  it("omits the ceilings for a legacy trace without them", () => {
    render(
      <MemoryRouter>
        <TraceDetail
          trace={{
            titleKey: "603",
            titleName: "Dune",
            profile: "1080p",
            at: "",
            candidates: [],
            searchDuration: 0,
          }}
        />
      </MemoryRouter>,
    );
    expect(screen.queryByText(/Profile ≤|Profile Any/)).not.toBeInTheDocument();
  });

  it("opens the release history tab", async () => {
    renderApp("/releases/history");
    expect(await screen.findByRole("heading", { name: "Releases" }, WAIT)).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "History" })).toHaveAttribute("aria-selected", "true");
  });
});

describe("Playback", () => {
  it("edits cache policy from the cache page", async () => {
    const { fake } = renderApp("/cache");
    expect(await screen.findByRole("heading", { name: "Cache" }, WAIT)).toBeInTheDocument();
    expect(await screen.findByRole("tab", { name: "Overview" }, WAIT)).toHaveAttribute(
      "aria-selected",
      "true",
    );
    expect(screen.queryByRole("button", { name: /Edit cache/i })).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Edit policy" }));
    fireEvent.click(screen.getAllByRole("switch")[0]!);
    fireEvent.click(screen.getByRole("button", { name: "Save policy" }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/cache")).toHaveLength(1));
  });

  it("shows detected hardware above the transcoding settings", async () => {
    renderApp("/playback/transcoding");
    expect(await screen.findByRole("tab", { name: "Transcoding" }, WAIT)).toHaveAttribute(
      "aria-selected",
      "true",
    );
    expect(await screen.findByText("Apple VideoToolbox", {}, WAIT)).toBeInTheDocument();
    expect(screen.getByText("H.264")).toBeInTheDocument();
    expect(screen.getByText("H.265/HEVC")).toBeInTheDocument();
    expect(screen.getByText("HDR10")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Hardware acceleration" })).toBeInTheDocument();
    expect(screen.getByText("RTX Video: Unavailable")).toBeInTheDocument();
  });

  it("identifies cached subtitles by real title, episode and download age, with explicit unknown titles", async () => {
    renderApp("/playback/subtitles");
    expect(
      await screen.findByText("Some Show · S01E02 · The Episode", {}, WAIT),
    ).toBeInTheDocument();
    expect(screen.getByText("Some.Show.S01E02.720p.WEB-DL-GROUP")).toBeInTheDocument();
    expect(screen.getByText("Unknown title")).toBeInTheDocument();
    expect(screen.getByText(/3 hours ago/)).toBeInTheDocument();
    expect(screen.getByText(/yesterday/)).toBeInTheDocument();
    // No cache hash or raw rating key is shown as identification.
    expect(screen.queryByText(/abc123|orphan99/)).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: /Adjust timing for Some Show/ }));
    expect(await screen.findByRole("dialog", {}, WAIT)).toBeInTheDocument();
    expect(screen.getByLabelText("Offset (milliseconds)")).toBeInTheDocument();
  });

  it("turns the intro skip button off and names IntroDB", async () => {
    const { fake } = renderApp("/playback/markers");
    expect(
      await screen.findByText("Intro and credit times from IntroDB.", {}, WAIT),
    ).toBeInTheDocument();
    expect(
      screen
        .getAllByRole("link", { name: "Integrations" })
        .some((link) => link.getAttribute("href")?.endsWith("/services/introdb")),
    ).toBe(true);
    expect(screen.getByText("Chapter titles")).toBeInTheDocument();
    expect(screen.getByText("Subtitle dialogue")).toBeInTheDocument();
    const intro = screen.getByRole("switch", { name: "Skip intro" });
    expect(intro).toBeChecked();
    fireEvent.click(intro);
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/markers")).toHaveLength(1), WAIT);
    expect(calls(fake, "PUT", "/config/markers")[0]!.body).toMatchObject({
      theIntroDb: true,
      skipIntro: false,
      skipCredits: true,
    });
  });

  it("saves nested server enhancement settings without losing transcoding preferences", async () => {
    const { fake } = renderApp("/playback/transcoding");
    await screen.findByRole("button", { name: "RTX Video Super Resolution" }, WAIT);
    expect(screen.queryByLabelText("ffmpeg path")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Enhancement quality")).not.toBeInTheDocument();
    await choose("RTX Video Super Resolution", "Automatic · local streams");
    fireEvent.click(screen.getByRole("button", { name: "More options: Sharper picture" }));
    await choose("Enhancement quality", "Streaming ultra");
    fireEvent.change(screen.getByLabelText("Enhanced resolution limit"), {
      target: { value: "1440" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/transcode")).toHaveLength(1), WAIT);
    expect(calls(fake, "PUT", "/config/transcode")[0]!.body).toMatchObject({
      hwAccel: "auto",
      rtxVideo: {
        mode: "auto",
        targetMaxHeight: 1440,
        sourceMaxHeight: 720,
        quality: 23,
        maxConcurrent: 1,
      },
    });
  });
});
