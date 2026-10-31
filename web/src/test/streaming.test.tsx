import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { describe, expect, it } from "vitest";
import type { EtherConfig, UsenetProvider } from "../lib/types";
import { normalizeTraces, resolutionCeilings, TraceDetail } from "../pages/releases/History";
import { calls, renderApp } from "./harness";

const WAIT = { timeout: 4000 };
async function choose(label: string, option: string) {
  fireEvent.click(screen.getByRole("button", { name: label }));
  fireEvent.click(await screen.findByRole("option", { name: option }));
}

describe("Sources", () => {
  it("enables bounded repair without changing provider credentials", async () => {
    const { fake } = renderApp("/sources/yours/usenet");
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
    const { fake } = renderApp("/sources/yours/usenet");
    fireEvent.click(await screen.findByRole("button", { name: "Edit Primary news" }, WAIT));
    fireEvent.change(screen.getByLabelText("Host"), { target: { value: "draft.example" } });
    fireEvent.click(screen.getByRole("button", { name: "Close" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    fireEvent.click(screen.getByRole("button", { name: "Edit Primary news" }));
    expect(await screen.findByLabelText("Host")).toHaveValue("news.example.com");
    expect(calls(fake, "PUT", "/config/usenet")).toHaveLength(0);
  });

  it("saves one provider from its drawer and keeps redacted secrets", async () => {
    const { fake } = renderApp("/sources/yours/usenet");
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
    const { fake } = renderApp("/sources/yours/usenet");
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
    const { fake } = renderApp("/sources/yours/usenet");
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
    renderApp("/sources/yours/indexers");
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
    fireEvent.change(screen.getByLabelText("Protocol"), { target: { value: "jellyfin" } });
    expect(screen.queryByText("Plex Web")).not.toBeInTheDocument();
    expect(screen.getByText("Jellyfin TV")).toBeInTheDocument();
    fireEvent.change(screen.getByLabelText("Device"), { target: { value: "Apple TV" } });
    expect(screen.queryByText("Jellyfin TV")).not.toBeInTheDocument();
    expect(screen.getByText("Swiftfin")).toBeInTheDocument();
    expect(screen.getByText("5, 8")).toBeInTheDocument();
  });
});

describe("Releases", () => {
  it("edits client rules in a drawer using a device and named viewer", async () => {
    const { fake } = renderApp("/releases/rules");
    expect(await screen.findByRole("button", { name: "Edit rule 1" }, WAIT)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Device" })).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Edit rule 1" }));
    const drawer = await screen.findByRole("dialog", { name: "Rule 1" });
    expect(within(drawer).getByRole("button", { name: "Device" })).toBeInTheDocument();
    await choose("Protocol", "Plex");
    await choose("Device", "Plex Web");
    await choose("Viewer", "The Kids");
    await choose("Profile", "1080p");
    fireEvent.click(screen.getByRole("button", { name: "Save rule" }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/quality")).toHaveLength(1));
    expect(calls(fake, "PUT", "/config/quality")[0]!.body).toMatchObject({
      rules: [{ protocol: "plex", product: "Plex Web", userId: 3, profile: "1080p" }],
    });
  });

  it("edits the default profiles in place with the save bar", async () => {
    const { fake } = renderApp("/releases/quality");
    expect(await screen.findByRole("button", { name: "Remote profile" }, WAIT)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Save changes" })).not.toBeInTheDocument();
    await choose("Remote profile", "4K");
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/quality")).toHaveLength(1));
    expect(calls(fake, "PUT", "/config/quality")[0]!.body).toMatchObject({
      default: "1080p",
      remote: "4K",
    });
  });

  it("renames a profile in its drawer and keeps rules pointing at it", async () => {
    const { fake } = renderApp("/releases/quality");
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
    const { fake } = renderApp("/releases/quality");
    fireEvent.click(await screen.findByRole("button", { name: "Edit 4K" }, WAIT));
    const drawer = await screen.findByRole("dialog", { name: "4K" });
    fireEvent.click(within(drawer).getByRole("button", { name: "Remove profile" }));
    expect(await screen.findByText(/The client rule using it will be removed/)).toBeInTheDocument();
    fireEvent.click(await screen.findByRole("button", { name: "Remove" }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/quality")).toHaveLength(1));
    const body = calls(fake, "PUT", "/config/quality")[0]!.body as EtherConfig["quality"];
    expect(body.profiles?.map((p) => p.name)).toEqual(["1080p"]);
    expect(body.rules).toEqual([]);
    expect(body).toMatchObject({ default: "1080p", remote: "1080p" });
  });

  it("discards invalid scoring drafts before editing startup settings", async () => {
    const { fake } = renderApp("/releases/scoring");
    const defs = await screen.findByLabelText("Custom format definitions", {}, WAIT);
    fireEvent.change(defs, { target: { value: "unfinished JSON" } });
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    expect(await screen.findByText(/aren't valid JSON/)).toBeInTheDocument();
    expect(calls(fake, "PUT", "/config/resolver")).toHaveLength(0);
    fireEvent.click(screen.getByRole("button", { name: "Discard" }));
    expect(screen.getByLabelText("Custom format definitions")).toHaveValue("[]");
    fireEvent.click(screen.getByRole("tab", { name: "Playback start" }));
    fireEvent.click(
      await screen.findByRole("switch", {
        name: "Start the top release before its health check completes",
      }),
    );
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/resolver")).toHaveLength(1));
    expect(calls(fake, "PUT", "/config/resolver")[0]!.body).toMatchObject({
      customFormats: [],
      optimisticStart: true,
    });
  });
});

describe("Source routing", () => {
  it("shows measured startup per source and saves the preference", async () => {
    const { fake } = renderApp("/releases/startup");
    expect(await screen.findByText(/debrid 410 ms \(22 plays\)/, {}, WAIT)).toBeInTheDocument();
    expect(screen.getByText(/Usenet 2\.1 s \(37 plays\)/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Source for equal-quality releases" }));
    fireEvent.click(await screen.findByRole("option", { name: "Prefer Usenet" }));
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/resolver")).toHaveLength(1));
    expect(calls(fake, "PUT", "/config/resolver")[0]!.body).toMatchObject({
      sourcePreference: "usenet",
    });
  });
});

describe("Release selection", () => {
  it("lets the owner set how many same-quality versions Plex lists", async () => {
    const { fake } = renderApp("/releases/selection");
    const field = await screen.findByLabelText("Same-quality versions to offer", {}, WAIT);
    expect(field).toHaveValue(1);
    fireEvent.change(field, { target: { value: "3" } });
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/resolver")).toHaveLength(1));
    expect(calls(fake, "PUT", "/config/resolver")[0]!.body).toMatchObject({
      sameQualityAlternatives: 3,
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
    renderApp("/playback");
    expect(await screen.findByRole("tab", { name: "Transcoding" }, WAIT)).toHaveAttribute(
      "aria-selected",
      "true",
    );
    expect(
      await screen.findByText("Hardware acceleration: videotoolbox", {}, WAIT),
    ).toBeInTheDocument();
    expect(screen.getByText("h264_videotoolbox")).toBeInTheDocument();
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

  it("saves nested server enhancement settings without losing transcoding preferences", async () => {
    const { fake } = renderApp("/playback");
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
