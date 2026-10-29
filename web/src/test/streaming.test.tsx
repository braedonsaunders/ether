import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { EtherConfig, UsenetProvider } from "../lib/types";
import { normalizeTraces } from "../pages/releases/History";
import { calls, renderApp } from "./harness";

const WAIT = { timeout: 4000 };
async function choose(label: string, option: string) {
  fireEvent.click(screen.getByRole("button", { name: label }));
  fireEvent.click(await screen.findByRole("option", { name: option }));
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

describe("Releases", () => {
  it("edits client rules in a drawer using a device and named viewer", async () => {
    const { fake } = renderApp("/releases/rules");
    expect(await screen.findByRole("button", { name: "Edit rule 1" }, WAIT)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Device" })).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Edit rule 1" }));
    const drawer = await screen.findByRole("dialog", { name: "Rule 1" });
    expect(within(drawer).getByRole("button", { name: "Device" })).toBeInTheDocument();
    await choose("Device", "Plex Web");
    await choose("Viewer", "The Kids");
    await choose("Profile", "1080p");
    fireEvent.click(screen.getByRole("button", { name: "Save rule" }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/quality")).toHaveLength(1));
    expect(calls(fake, "PUT", "/config/quality")[0]!.body).toMatchObject({
      rules: [{ product: "Plex Web", userId: 3, profile: "1080p" }],
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

describe("Release traces", () => {
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

  it("opens the release history tab", async () => {
    renderApp("/releases/history");
    expect(await screen.findByRole("heading", { name: "Releases" }, WAIT)).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "History" })).toHaveAttribute("aria-selected", "true");
  });
});

describe("Playback", () => {
  it("redirects the legacy cache URL to the new management page", async () => {
    const { fake } = renderApp("/playback/cache");
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

  it("identifies cached subtitles by release, title key and download age, and opens the sync drawer", async () => {
    renderApp("/playback/subtitles");
    expect(
      await screen.findByText("Some.Movie.2024.1080p.WEB-DL-GROUP", {}, WAIT),
    ).toBeInTheDocument();
    expect(screen.getByText(/Title key 1000000000000550 · abc123\.srt/)).toBeInTheDocument();
    expect(screen.getByText("pt-br")).toBeInTheDocument();
    expect(screen.getByText(/3 hours ago/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: /Adjust timing for Some\.Movie/ }));
    expect(await screen.findByRole("dialog", {}, WAIT)).toBeInTheDocument();
    expect(screen.getByLabelText("Offset (milliseconds)")).toBeInTheDocument();
  });

  it("saves nested server enhancement settings without losing transcoding preferences", async () => {
    const { fake } = renderApp("/playback");
    await screen.findByRole("button", { name: "RTX Video Super Resolution" }, WAIT);
    await choose("RTX Video Super Resolution", "Automatic · local streams");
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
