import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import type { TasteOverride } from "../lib/contract";
import type { FakeServer } from "../mock/server";
import { calls, renderApp } from "./harness";

const WAIT = { timeout: 4000 };

type Route = (method: string, path: string, body: unknown) => unknown;

/**
 * Serve endpoints the shared fake doesn't model (safety policies).
 * The fake still records every call; only its 404s are replaced.
 */
function extraRoutes(route: Route) {
  return (fake: FakeServer) => {
    const base = fake.fetch;
    fake.fetch = async (input, init) => {
      const res = await base(input, init);
      if (res.status !== 404) return res;
      const url = new URL(String(input), "http://localhost");
      const path = url.pathname.replace(/^\/ether\/api/, "") + url.search;
      const raw = typeof init?.body === "string" && init.body ? init.body : "";
      const out = route((init?.method ?? "GET").toUpperCase(), path, raw ? JSON.parse(raw) : null);
      if (out === undefined) return res;
      return new Response(JSON.stringify(out), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      });
    };
  };
}

const POLICY = {
  blockedGenres: [],
  blockedKeywords: [],
  remoteDisabled: false,
  maxBitrateKbps: 0,
  allowedHours: [],
  timezone: "UTC",
};

const withPlexDefaults = (server: FakeServer) => {
  server.state.plexDefaults[3] = {
    hasProfile: true,
    hasSettings: true,
    audioLanguage: "ja",
    autoSelectAudio: true,
    subtitleLanguage: "en",
    subtitleMode: "always",
    filterMovies: "contentRating<=PG-13",
    filterTelevision: "label!=Hidden",
    filterMoviesParts: [{ kind: "rating-max", values: ["PG-13"] }],
    filterTelevisionParts: [{ kind: "label-deny", values: ["Hidden"] }],
    allowSync: false,
  };
};

describe("Viewers", () => {
  it("lists viewers with activity and opens a profile", async () => {
    renderApp("/viewers");
    const row = await screen.findByText("Sam Rivera", {}, WAIT);
    expect(screen.getByText("The Kids")).toBeInTheDocument();
    // The kids' rating cap shows in the limits summary.
    expect(await screen.findByText("Up to PG/TV-PG/TV-Y7", {}, WAIT)).toBeInTheDocument();
    fireEvent.click(row);
    expect(await screen.findByRole("heading", { name: /Sam Rivera/ }, WAIT)).toBeInTheDocument();
    // Overview is the landing tab now.
    expect(await screen.findByText("Recent activity", {}, WAIT)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("tab", { name: "Taste" }));
    expect(await screen.findByText("What Ether has learned", {}, WAIT)).toBeInTheDocument();
    expect(await screen.findByText("Up next", {}, WAIT)).toBeInTheDocument();
    expect(await screen.findByText("Pick 1", {}, WAIT)).toBeInTheDocument();
    expect(await screen.findByText("Pick 2", {}, WAIT)).toBeInTheDocument();
  });

  it("corrects taste from the genre chart and saves overrides", async () => {
    const { fake } = renderApp("/viewers/2/taste");
    fireEvent.click(await screen.findByRole("tab", { name: "Genres" }));
    const genres = await screen.findByRole("list", { name: /Genre affinity/ }, WAIT);
    fireEvent.click(within(genres).getByRole("button", { name: /Horror/ }));
    fireEvent.click(await screen.findByRole("button", { name: "Not into it" }));
    expect(screen.getByRole("dialog", { name: "Horror" })).toHaveTextContent("Not into it");
    fireEvent.click(screen.getByRole("button", { name: /Save correction/ }));
    await waitFor(
      () => expect(calls(fake, "PUT", "/viewers/2/taste/overrides")).toHaveLength(1),
      WAIT,
    );
    const body = calls(fake, "PUT", "/viewers/2/taste/overrides")[0]!.body as {
      overrides: TasteOverride[];
    };
    expect(body.overrides).toEqual([{ kind: "genre", value: "Horror", weight: -1 }]);
    // The returned profile now carries the override; nothing is left unsaved.
    await waitFor(() =>
      expect(screen.getByRole("button", { name: /Save correction/ })).toBeDisabled(),
    );
  });

  it("adds a person correction through search", async () => {
    const { fake } = renderApp("/viewers/1/taste");
    fireEvent.click(await screen.findByRole("tab", { name: "Corrections" }));
    await screen.findByText("Your corrections", {}, WAIT);
    fireEvent.click(screen.getByRole("radio", { name: "Person" }));
    fireEvent.click(screen.getByRole("radio", { name: "Loves it" }));
    const search = screen.getByRole("combobox", { name: "Person to correct" });
    fireEvent.focus(search);
    fireEvent.change(search, { target: { value: "Ada" } });
    fireEvent.click(await screen.findByRole("button", { name: /Ada Moreau/ }, WAIT));
    fireEvent.click(await screen.findByRole("button", { name: /Save changes/ }));
    await waitFor(
      () => expect(calls(fake, "PUT", "/viewers/1/taste/overrides")).toHaveLength(1),
      WAIT,
    );
    expect(
      (calls(fake, "PUT", "/viewers/1/taste/overrides")[0]!.body as { overrides: TasteOverride[] })
        .overrides,
    ).toEqual([{ kind: "person", value: "108", weight: 1 }]);
  });

  it("sets a viewer's streams at once on their user record", async () => {
    const { fake } = renderApp("/viewers/4/limits");
    const limit = await screen.findByLabelText("Streams at once", {}, WAIT);
    // One stream limit and one rating limit; no second copies, no account languages.
    expect(screen.getAllByLabelText("Streams at once")).toHaveLength(1);
    expect(screen.getAllByLabelText("Content rating limit")).toHaveLength(1);
    expect(screen.queryByLabelText("Concurrent streams")).toBeNull();
    expect(screen.queryByText("Account languages")).toBeNull();
    expect(screen.getByRole("link", { name: "Change the server default" })).toHaveAttribute(
      "href",
      "/playback/transcoding",
    );
    fireEvent.change(limit, { target: { value: "1" } });
    fireEvent.click(await screen.findByRole("button", { name: /Save changes/ }));
    await waitFor(() => expect(calls(fake, "PATCH", "/users/4")).toHaveLength(1), WAIT);
    expect(calls(fake, "PATCH", "/users/4")[0]!.body).toEqual({
      streamLimit: 1,
      maxContentRating: "",
    });
    // Only the part that changed is written.
    expect(calls(fake, "PUT", "/safety/policies/4")).toHaveLength(0);
  });

  it("picks the allowed-times timezone from a searchable list", async () => {
    const { fake } = renderApp("/viewers/4/limits");
    fireEvent.click(await screen.findByLabelText("Playback timezone", {}, WAIT));
    const search = await screen.findByRole("textbox", { name: "Search…" }, WAIT);
    fireEvent.change(search, { target: { value: "Tokyo" } });
    fireEvent.click(await screen.findByRole("option", { name: "Asia/Tokyo" }, WAIT));
    fireEvent.click(await screen.findByRole("button", { name: /Save changes/ }));
    await waitFor(() => expect(calls(fake, "PUT", "/safety/policies/4")).toHaveLength(1), WAIT);
    expect(calls(fake, "PUT", "/safety/policies/4")[0]!.body).toMatchObject({
      timezone: "Asia/Tokyo",
    });
  });

  it("keeps the Live TV rating behind More options and links Simkl setup", async () => {
    renderApp("/viewers/4/limits");
    const more = await screen.findByRole("button", { name: "More options: Live TV" }, WAIT);
    expect(screen.queryByLabelText("Different limit for Live TV")).toBeNull();
    fireEvent.click(more);
    expect(await screen.findByLabelText("Different limit for Live TV")).toBeInTheDocument();
    // Simkl is off in the fake, so the viewer page links to its setup.
    const hint = await screen.findByText(/to connect this viewer's account/, {}, WAIT);
    expect(within(hint).getByRole("link", { name: "Integrations" })).toHaveAttribute(
      "href",
      "/services/simkl",
    );
  });

  it("saves a viewer's parental controls from their Access tab", async () => {
    const { fake } = renderApp(
      "/viewers/3/limits",
      {},
      extraRoutes((method, path) => {
        if (method === "GET" && path === "/safety/policies/3") return POLICY;
        if (method === "PUT" && path === "/safety/policies/3") return { ok: true };
        return undefined;
      }),
    );
    fireEvent.click(await screen.findByRole("button", { name: "Teens" }, WAIT));
    await waitFor(
      () =>
        expect(screen.getByLabelText("Blocked keywords")).toHaveValue("pornography, erotic movie"),
      WAIT,
    );
    fireEvent.change(screen.getByLabelText("Blocked genres"), {
      target: { value: "Horror, Thriller" },
    });
    fireEvent.click(screen.getByLabelText("Local network only"));
    fireEvent.change(screen.getByLabelText("Streams at once"), { target: { value: "2" } });
    fireEvent.click(
      within(screen.getByRole("group", { name: "Allowed hours" })).getByRole("button", {
        name: "20:00",
      }),
    );
    fireEvent.click(screen.getByRole("button", { name: /Save changes/ }));
    await waitFor(() => expect(calls(fake, "PUT", "/safety/policies/3")).toHaveLength(1), WAIT);
    const saved = calls(fake, "PUT", "/safety/policies/3")[0]!.body as Record<string, unknown>;
    expect(saved).toMatchObject({
      blockedGenres: ["Horror", "Thriller"],
      blockedKeywords: ["pornography", "erotic movie"],
      remoteDisabled: true,
      allowedHours: [20],
    });
    expect(saved).not.toHaveProperty("maxContentRating");
    expect(saved).not.toHaveProperty("streamLimit");
    // The preset's rating limit and the stream limit go to the user record.
    await waitFor(() => expect(calls(fake, "PATCH", "/users/3")).toHaveLength(1), WAIT);
    expect(calls(fake, "PATCH", "/users/3")[0]!.body).toMatchObject({
      maxContentRating: "PG-13",
      streamLimit: 2,
    });
    await waitFor(() => expect(screen.queryByText("You have unsaved changes")).toBeNull());
  });

  it("shows read-only plex.tv share defaults on Access", async () => {
    renderApp("/viewers/3/limits", {}, withPlexDefaults);
    expect(await screen.findByText("From Plex", {}, WAIT)).toBeInTheDocument();
    expect(screen.getByText("Up to PG-13")).toBeInTheDocument();
    expect(screen.getByText("Hides collection Hidden")).toBeInTheDocument();
  });

  it("shows read-only plex.tv track defaults on Playback", async () => {
    renderApp("/viewers/3/playback", { householdAudio: true }, (fake) => {
      withPlexDefaults(fake);
      // Viewer 3 has no stream override of their own.
      extraRoutes((_method, path) => {
        if (path === "/stream-preferences/3") return { audioLanguages: [], subtitleLanguages: [] };
        if (path === "/stream-preferences/3/shows") return [];
        return undefined;
      })(fake);
    });
    expect(await screen.findByText("From Plex", {}, WAIT)).toBeInTheDocument();
    expect(await screen.findByText("ja", {}, WAIT)).toBeInTheDocument();
    // The Playback page also offers "Always on" as a subtitle-mode option;
    // the From Plex row renders it as plain text.
    expect(screen.getAllByText("Always on").some((el) => el.tagName === "SPAN")).toBe(true);
  });
});

it("filters imported history without changing Plex access", async () => {
  const { fake } = renderApp("/viewers", {}, (server) => {
    const base = server.fetch;
    server.fetch = async (input, init) => {
      const res = await base(input, init);
      const url = new URL(String(input), "http://localhost");
      if (url.pathname !== "/ether/api/viewers") return res;
      const body = await res.json();
      body.viewers = body.viewers.map((v: { id: number; isOwner: boolean }) => ({
        ...v,
        accessStatus: v.isOwner ? "owner" : v.id === 2 ? "imported" : "active",
        ...(v.id === 2
          ? {
              lifecycle: {
                source: "local",
                paused: false,
                managedBy: "",
                canPause: true,
                canRemove: true,
                canMerge: true,
              },
            }
          : {}),
      }));
      return new Response(JSON.stringify(body), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      });
    };
  });
  // The list opens on Active, so imported history is hidden until All.
  expect(await screen.findByText("The Kids", {}, WAIT)).toBeInTheDocument();
  expect(screen.queryByText("Sam Rivera")).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: /^All \(/ }));
  expect(screen.getByText("Sam Rivera")).toBeInTheDocument();
  expect(screen.getByText("Imported history", { selector: "span" })).toBeInTheDocument();
  expect(screen.getByText("The Kids")).toBeInTheDocument();
  expect(screen.getByRole("link", { name: "Plex.tv" })).toHaveAttribute(
    "href",
    "https://app.plex.tv/desktop/#!/settings/manage-library-access",
  );
  expect(
    fake.state.calls.filter((c) => ["POST", "PUT", "PATCH", "DELETE"].includes(c.method)),
  ).toHaveLength(0);
});

/** Override the lifecycle of the seed viewers by id. */
function withLifecycles(
  byId: Record<number, Record<string, unknown>>,
): (server: FakeServer) => void {
  return (server) => {
    const base = server.fetch;
    server.fetch = async (input, init) => {
      const res = await base(input, init);
      const url = new URL(String(input), "http://localhost");
      if (url.pathname !== "/ether/api/viewers") return res;
      const body = await res.json();
      body.viewers = body.viewers.map((v: { id: number; lifecycle: unknown }) => ({
        ...v,
        ...(byId[v.id] ? { lifecycle: byId[v.id] } : {}),
      }));
      return new Response(JSON.stringify(body), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      });
    };
  };
}

it("puts the owner first as You and filters by lifecycle", async () => {
  renderApp(
    "/viewers",
    {},
    withLifecycles({
      2: {
        source: "local",
        paused: true,
        managedBy: "",
        canPause: true,
        canRemove: true,
        canMerge: true,
      },
      4: {
        source: "plex",
        paused: false,
        plexShared: false,
        managedBy: "plex",
        canPause: false,
        canRemove: false,
        canMerge: false,
      },
    }),
  );
  // Active is the first filter and the default; paused viewers wait under All.
  const active = await screen.findByRole("button", { name: /^Active \(/ }, WAIT);
  expect(screen.getAllByRole("button", { name: /^(Active|All|Paused) \(/ })[0]).toBe(active);
  await screen.findByText("You", {}, WAIT);
  expect(screen.queryByText("Sam Rivera")).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: /^All \(/ }));
  await screen.findByText("Sam Rivera", {}, WAIT);
  // The first data row is the owner, shown as "You" with an Owner badge.
  const rows = screen.getAllByRole("row");
  expect(rows[1]).toHaveTextContent("You");
  expect(rows[1]).toHaveTextContent("Owner");
  expect(rows[1]).toHaveTextContent("Alex Rivera");
  // Status and source badges from the lifecycle.
  expect(screen.getByText("Paused", { selector: "span" })).toBeInTheDocument();
  expect(screen.getByText("Local", { selector: "span" })).toBeInTheDocument();
  expect(screen.getByText("No longer shared", { selector: "span" })).toBeInTheDocument();
  expect(screen.getAllByText("Managed on plex.tv", { selector: "span" }).length).toBeGreaterThan(0);
  // Paused chip keeps only the paused local viewer.
  fireEvent.click(screen.getByRole("button", { name: /^Paused \(/ }));
  expect(screen.getByText("Sam Rivera")).toBeInTheDocument();
  expect(screen.queryByText("Jordan Lee")).not.toBeInTheDocument();
  expect(screen.queryByText("You")).not.toBeInTheDocument();
  // From Plex chip (Plex is connected in the fake) keeps only Plex rows.
  fireEvent.click(screen.getByRole("button", { name: /^From Plex \(/ }));
  expect(screen.getByText("Jordan Lee")).toBeInTheDocument();
  expect(screen.getByText("The Kids")).toBeInTheDocument();
  expect(screen.queryByText("Sam Rivera")).not.toBeInTheDocument();
});

it("shows nothing Plex-related when Plex is off", async () => {
  renderApp("/viewers", { bootstrap: { plexEnabled: false } });
  await screen.findByText("Sam Rivera", {}, WAIT);
  expect(screen.queryByRole("button", { name: /^From Plex \(/ })).not.toBeInTheDocument();
  expect(screen.queryByText("Managed on plex.tv")).not.toBeInTheDocument();
  expect(screen.queryByRole("link", { name: "Plex.tv" })).not.toBeInTheDocument();
});

it("adds a local viewer through the guided flow and shows how to connect", async () => {
  const { fake } = renderApp("/viewers", {}, (server) => {
    server.state.config.frontends = {
      plex: { enabled: true },
      jellyfin: { enabled: true },
      stremio: { enabled: true },
    };
  });
  const writeText = vi.fn(async (_text: string) => {});
  Object.assign(navigator, { clipboard: { writeText } });
  await screen.findByText("Sam Rivera", {}, WAIT);
  fireEvent.click(screen.getByRole("button", { name: /Add viewer/ }));
  const drawer = await screen.findByRole("dialog", { name: "Add viewer" }, WAIT);
  fireEvent.change(within(drawer).getByLabelText("Their name"), { target: { value: "Riley" } });
  fireEvent.click(within(drawer).getByRole("button", { name: "Continue" }));
  expect(await within(drawer).findByText("How will they sign in?", {}, WAIT)).toBeInTheDocument();
  expect(within(drawer).getByLabelText("Username")).toHaveValue("riley");
  fireEvent.change(within(drawer).getByLabelText("Password"), {
    target: { value: "a-very-long-password" },
  });
  const create = within(drawer).getByRole("button", { name: "Create viewer" });
  const confirmation = within(drawer).getByLabelText("Confirm password");
  expect(create).toBeDisabled();
  fireEvent.change(confirmation, { target: { value: "a-different-password" } });
  expect(confirmation).toHaveAttribute("aria-invalid", "true");
  expect(confirmation).toHaveAccessibleDescription("Passwords do not match.");
  expect(create).toBeDisabled();
  fireEvent.submit(create.closest("form")!);
  expect(calls(fake, "POST", "/users/local")).toHaveLength(0);
  fireEvent.change(confirmation, { target: { value: "a-very-long-password" } });
  expect(confirmation).not.toHaveAttribute("aria-invalid");
  expect(within(drawer).queryByText("Passwords do not match.")).toBeNull();
  expect(create).toBeEnabled();
  fireEvent.click(create);
  expect(await within(drawer).findByText("How to connect", {}, WAIT)).toBeInTheDocument();
  await waitFor(() => expect(calls(fake, "POST", "/users/local")).toHaveLength(1), WAIT);
  expect(calls(fake, "POST", "/users/local")[0]!.body).toEqual({
    username: "riley",
    password: "a-very-long-password",
  });
  // One card per enabled app, plus the plain Plex truth for local viewers.
  expect(within(drawer).getByRole("heading", { name: "Jellyfin" })).toBeInTheDocument();
  expect(within(drawer).getByText(/They sign in as riley\./)).toBeInTheDocument();
  expect(within(drawer).getByText(/Quick Connect/)).toBeInTheDocument();
  expect(within(drawer).getByText(/Infuse, Swiftfin, Findroid, Streamyfin/)).toBeInTheDocument();
  expect(within(drawer).getByRole("heading", { name: "Stremio" })).toBeInTheDocument();
  expect(within(drawer).getByText(/They get their link at .*\/ether\/me/)).toBeInTheDocument();
  expect(
    within(drawer).getByText("Local viewers can't use Plex apps unless they link a Plex account."),
  ).toBeInTheDocument();
  fireEvent.click(within(drawer).getByRole("button", { name: /Copy as message/ }));
  await waitFor(() => expect(writeText).toHaveBeenCalledOnce(), WAIT);
  const message = String(writeText.mock.calls[0]![0]);
  expect(message).toContain("Hi Riley, you can watch on my Ether server.");
  expect(message).toContain("Your username is riley.");
  expect(message).toContain("Jellyfin:");
  fireEvent.click(within(drawer).getByRole("button", { name: "Done" }));
  // The new viewer joins the list once the drawer closes.
  await waitFor(() => expect(screen.getAllByText("riley").length).toBeGreaterThan(0), WAIT);
});

it("hides server-only sections for a viewer kept for imported history", async () => {
  renderApp("/viewers/2/taste", {}, (server) => {
    const base = server.fetch;
    server.fetch = async (input, init) => {
      const res = await base(input, init);
      const url = new URL(String(input), "http://localhost");
      if (url.pathname !== "/ether/api/viewers/2") return res;
      const body = await res.json();
      if (body.viewer) body.viewer.accessStatus = "imported";
      else body.accessStatus = "imported";
      return new Response(JSON.stringify(body), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      });
    };
  });
  const tabs = await screen.findByRole("tablist", { name: "Viewer sections" }, WAIT);
  expect(within(tabs).queryByRole("tab", { name: "Watchlist" })).toBeNull();
  expect(within(tabs).queryByRole("tab", { name: "Alerts" })).toBeNull();
  expect(within(tabs).getByRole("tab", { name: "Taste" })).toHaveAttribute("aria-selected", "true");
  // The watchlist and alerts sections inside Taste need server access.
  await screen.findByText("What Ether has learned", {}, WAIT);
  expect(screen.queryByText("New episodes from favourite shows")).toBeNull();
});

it("shows a bounded dashboard and ordered viewer sections", async () => {
  renderApp("/viewers/2/overview");
  const tabs = await screen.findByRole("tablist", { name: "Viewer sections" }, WAIT);
  expect(
    within(tabs)
      .getAllByRole("tab")
      .map((tab) => tab.textContent),
  ).toEqual([
    "Overview",
    "Home",
    "Taste",
    "Recent activity",
    "Audio & subtitles",
    "Limits",
    "Sign-in & devices",
    "Lists & alerts",
    "Fixture viewer.tab",
  ]);
  expect(screen.getByLabelText("Viewer dashboard")).toHaveClass("overflow-hidden");
  expect(screen.getByText("Plays in 30 days")).toBeInTheDocument();
  expect(screen.queryByText("How they sign in")).toBeNull();
  fireEvent.click(within(tabs).getByRole("tab", { name: "Recent activity" }));
  expect(await screen.findByRole("table")).toBeInTheDocument();
});

it("deep-links to list sub-tabs and restores hidden recommendations", async () => {
  const { fake } = renderApp("/viewers/4/lists?list=hidden");
  const tabs = await screen.findByRole("tablist", { name: "Lists and alerts sections" }, WAIT);
  expect(within(tabs).getByRole("tab", { name: "Not interested" })).toHaveAttribute(
    "aria-selected",
    "true",
  );
  const restore = await screen.findAllByRole(
    "button",
    { name: /Show .* in recommendations/ },
    WAIT,
  );
  fireEvent.click(restore[0]!);
  await waitFor(() =>
    expect(calls(fake, "POST", "/viewers/4/not-interested/restore")).toHaveLength(1),
  );
  fireEvent.click(within(tabs).getByRole("tab", { name: "Alerts" }));
  expect(await screen.findByText("New episodes from favourite shows")).toBeInTheDocument();
  expect(screen.queryByRole("switch")).toBeNull();
  fireEvent.click(within(tabs).getByRole("tab", { name: "Watchlist" }));
  expect(
    await screen.findByRole("switch", { name: "Show my favourites as a playlist in Plex." }),
  ).toBeInTheDocument();
});

it("imports viewers from connected Plex servers via drawer", async () => {
  const candidateServers: Array<{
    serverId: number;
    serverName: string;
    accounts: Array<{
      serverId: number;
      serverName: string;
      accountId: number;
      accountName: string;
      isOwner: boolean;
      matchedViewerId: number;
      matchedViewerName?: string;
    }>;
  }> = [
    {
      serverId: 1,
      serverName: "Old Plex Server",
      accounts: [
        {
          serverId: 1,
          serverName: "Old Plex Server",
          accountId: 42,
          accountName: "PlexFriend",
          isOwner: false,
          matchedViewerId: 0,
        },
      ],
    },
  ];

  const { fake } = renderApp(
    "/viewers",
    {},
    extraRoutes((method, path) => {
      if (path === "/viewers/plex-candidates" && method === "GET") {
        return candidateServers;
      }
      if (path === "/viewers/import-plex" && method === "POST") {
        candidateServers[0]!.accounts[0]!.matchedViewerId = 99;
        candidateServers[0]!.accounts[0]!.matchedViewerName = "PlexFriend";
        return { id: 99, name: "PlexFriend", inviteUrl: "/invite/test-token" };
      }
      return undefined;
    }),
  );

  const importBtn = await screen.findByRole("button", { name: /Import from Plex/i }, WAIT);
  fireEvent.click(importBtn);

  const drawer = await screen.findByRole("dialog", { name: /Import users from connected Plex servers/i }, WAIT);
  expect(drawer).toBeInTheDocument();
  expect(await within(drawer).findByText("Old Plex Server", {}, WAIT)).toBeInTheDocument();
  expect(within(drawer).getByText("PlexFriend")).toBeInTheDocument();

  const addViewerBtn = within(drawer).getByRole("button", { name: /Add as viewer/i });
  fireEvent.click(addViewerBtn);

  await waitFor(() => {
    expect(calls(fake, "POST", "/viewers/import-plex")).toHaveLength(1);
  }, WAIT);
  expect(calls(fake, "POST", "/viewers/import-plex")[0]!.body).toEqual({
    serverId: 1,
    accountId: 42,
    name: "PlexFriend",
  });

  // Verify invite link is displayed after import
  expect(await within(drawer).findByText(/\/invite\/test-token/, {}, WAIT)).toBeInTheDocument();

  // Test search filtering by Plex username
  const searchInput = within(drawer).getByPlaceholderText(/Search by account name or Plex username/i);
  fireEvent.change(searchInput, { target: { value: "NonExistent" } });
  expect(within(drawer).queryByText("PlexFriend")).toBeNull();
  expect(within(drawer).getByText(/No accounts match "NonExistent"/i)).toBeInTheDocument();

  // Verify accessibility and external link to Plex.tv
  expect(searchInput).toHaveAttribute("aria-label", "Search by account name or Plex username");
  const manageLink = within(drawer).getByRole("link", { name: /Manage Library Access/i });
  expect(manageLink).toHaveAttribute("href", "https://app.plex.tv/desktop/#!/settings/manage-library-access");

  fireEvent.change(searchInput, { target: { value: "PlexFriend" } });
  expect(within(drawer).getByText("PlexFriend")).toBeInTheDocument();
});
