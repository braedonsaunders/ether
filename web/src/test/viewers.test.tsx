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
    expect(
      await screen.findByRole("heading", { name: /Sam Rivera/ }, { timeout: 10000 }),
    ).toBeInTheDocument();
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

  it("shows plain-language affinity labels, filters sensitive/generic themes, and explains corrections", async () => {
    renderApp("/viewers/2/taste", {}, (server) => {
      const base = server.fetch;
      server.fetch = async (input, init) => {
        const res = await base(input, init);
        const url = new URL(String(input), "http://localhost");
        if (url.pathname !== "/ether/api/viewers/2/taste") return res;
        const body = await res.json();
        body.genres = [
          { name: "Science Fiction", weight: 0.82 },
          { name: "Horror", weight: -0.35 },
        ];
        body.keywords = [
          { name: "time travel", weight: 0.72 },
          { name: "rape", weight: 0.85 },
          { name: "suicide", weight: 0.65 },
          { name: "based on novel or book", weight: 0.9 },
        ];
        body.languages = [{ code: "ja", weight: 0.45 }];
        return new Response(JSON.stringify(body), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        });
      };
    });

    fireEvent.click(await screen.findByRole("tab", { name: "Genres" }, WAIT));
    const genres = await screen.findByRole("list", { name: /Genre affinity/ }, WAIT);
    expect(within(genres).getByText("Loves")).toBeInTheDocument();
    expect(within(genres).getByText("Dislikes")).toBeInTheDocument();
    expect(within(genres).queryByText("+0.82")).toBeNull();
    expect(within(genres).queryByText("-0.35")).toBeNull();

    fireEvent.click(screen.getByRole("tab", { name: "Themes & languages" }));
    expect(await screen.findByText("time travel", {}, WAIT)).toBeInTheDocument();
    expect(screen.queryByText("rape")).toBeNull();
    expect(screen.queryByText("suicide")).toBeNull();
    expect(screen.queryByText("based on novel or book")).toBeNull();
    expect(screen.getByText("Likes")).toBeInTheDocument();
    expect(screen.queryByText("+0.45")).toBeNull();

    fireEvent.click(screen.getByRole("tab", { name: "Corrections" }));
    expect(
      await screen.findByText(
        "“Not into it” hides it from recommendations. “Loves it” pushes it higher.",
        {},
        WAIT,
      ),
    ).toBeInTheDocument();
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

  it("hides parental controls on the owner's Limits tab", async () => {
    renderApp("/viewers/1/limits");
    expect(await screen.findByLabelText("Streams at once", {}, WAIT)).toBeInTheDocument();
    expect(screen.queryByText("Parental controls")).toBeNull();
    expect(screen.queryByText("Start from a preset")).toBeNull();
    expect(screen.queryByLabelText("Content rating limit")).toBeNull();
    expect(screen.queryByLabelText("Blocked genres")).toBeNull();
    expect(screen.queryByLabelText("Blocked keywords")).toBeNull();
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

  it("keeps Live TV rating behind More options on Limits and moves connected accounts to Sign-in & devices without Invite", async () => {
    renderApp("/viewers/4/limits");
    const more = await screen.findByRole("button", { name: "More options: Live TV" }, WAIT);
    expect(screen.queryByLabelText("Different limit for Live TV")).toBeNull();
    fireEvent.click(more);
    expect(await screen.findByLabelText("Different limit for Live TV")).toBeInTheDocument();
    // Connected-account sync sections no longer live under Limits.
    expect(screen.queryByRole("heading", { name: "Simkl" })).toBeNull();
    expect(screen.queryByRole("heading", { name: "Trakt" })).toBeNull();

    // Sign-in & devices omits the new-viewer Invite card and houses Connected accounts.
    fireEvent.click(screen.getByRole("tab", { name: "Sign-in & devices" }));
    const signInTabs = await screen.findByRole(
      "tablist",
      { name: "Sign-in and devices sections" },
      WAIT,
    );
    expect(screen.queryByRole("heading", { name: "Invite" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Create invite link" })).toBeNull();

    fireEvent.click(within(signInTabs).getByRole("tab", { name: "Connected accounts" }));
    const simklSection = (await screen.findByText("Simkl", { selector: "h2" }, WAIT)).closest(
      "section",
    )!;
    const hint = within(simklSection).getByText(/to connect this viewer's account/);
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
    expect(screen.getByLabelText("Bedtime")).toBeChecked();
    fireEvent.change(screen.getByLabelText("Bedtime starts"), { target: { value: "21:30" } });
    fireEvent.change(screen.getByLabelText("Wake time"), { target: { value: "07:15" } });
    const days = within(screen.getByRole("group", { name: "Days" }));
    fireEvent.click(days.getByRole("button", { name: "Monday" }));
    fireEvent.click(days.getByRole("button", { name: "Tuesday" }));
    fireEvent.click(screen.getByRole("button", { name: /Save changes/ }));
    await waitFor(() => expect(calls(fake, "PUT", "/safety/policies/3")).toHaveLength(1), WAIT);
    const saved = calls(fake, "PUT", "/safety/policies/3")[0]!.body as Record<string, unknown>;
    expect(saved).toMatchObject({
      blockedGenres: ["Horror", "Thriller"],
      blockedKeywords: ["pornography", "erotic movie"],
      remoteDisabled: true,
      allowedHours: [],
      bedtime: { time: "21:30", wakeTime: "07:15", days: [1, 2], timezone: "" },
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
    expect(await screen.findByText("Japanese", {}, WAIT)).toBeInTheDocument();
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

it("shows an unbounded responsive dashboard and ordered viewer sections", async () => {
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
  const dashboard = screen.getByLabelText("Viewer dashboard");
  expect(dashboard).not.toHaveClass("overflow-hidden");
  expect(dashboard.firstElementChild).toHaveClass("grid-cols-1", "sm:grid-cols-3");
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

  const drawer = await screen.findByRole(
    "dialog",
    { name: /Import users from connected Plex servers/i },
    WAIT,
  );
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
  const searchInput = within(drawer).getByPlaceholderText(
    /Search by account name or Plex username/i,
  );
  fireEvent.change(searchInput, { target: { value: "NonExistent" } });
  expect(within(drawer).queryByText("PlexFriend")).toBeNull();
  expect(within(drawer).getByText(/No accounts match "NonExistent"/i)).toBeInTheDocument();

  // Verify accessibility and external link to Plex.tv
  expect(searchInput).toHaveAttribute("aria-label", "Search by account name or Plex username");
  const manageLink = within(drawer).getByRole("link", { name: /Manage Library Access/i });
  expect(manageLink).toHaveAttribute(
    "href",
    "https://app.plex.tv/desktop/#!/settings/manage-library-access",
  );

  fireEvent.change(searchInput, { target: { value: "PlexFriend" } });
  expect(within(drawer).getByText("PlexFriend")).toBeInTheDocument();
});

it("keeps suggested match selectable from Match existing dropdown", async () => {
  const candidateServers = [
    {
      serverId: 1,
      serverName: "Old Plex Server",
      accounts: [
        {
          serverId: 1,
          serverName: "Old Plex Server",
          accountId: 42,
          accountName: "Sam Rivera",
          isOwner: false,
          matchedViewerId: 0,
          suggestedViewerId: 2,
          suggestedViewerName: "Sam Rivera",
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
      if (path === "/viewers/match-plex" && method === "POST") {
        candidateServers[0]!.accounts[0]!.matchedViewerId = 2;
        return { ok: true, viewerId: 2, serverId: 1, accountId: 42, sharedPlex: true };
      }
      return undefined;
    }),
  );

  fireEvent.click(await screen.findByRole("button", { name: /Import from Plex/i }, WAIT));
  const drawer = await screen.findByRole(
    "dialog",
    { name: /Import users from connected Plex servers/i },
    WAIT,
  );
  expect(
    await within(drawer).findByText("Suggested match: Sam Rivera", {}, WAIT),
  ).toBeInTheDocument();

  const trigger = within(drawer).getByRole("button", {
    name: "Match account to existing viewer",
  });
  expect(trigger).toHaveTextContent("Match existing…");
  const select = drawer.querySelector("select") as HTMLSelectElement;
  expect(select.value).toBe("0");

  fireEvent.click(trigger);
  fireEvent.click(await screen.findByRole("option", { name: /Sam Rivera/ }, WAIT));
  await waitFor(() => {
    expect(calls(fake, "POST", "/viewers/match-plex")).toHaveLength(1);
  }, WAIT);
  expect(calls(fake, "POST", "/viewers/match-plex")[0]!.body).toEqual({
    serverId: 1,
    accountId: 42,
    viewerId: 2,
  });
});

it("splits Plex import drawer by Plex.tv relationship and history", async () => {
  const candidateServers = [
    {
      serverId: 1,
      serverName: "One.Plex",
      plexDirectoryTotal: 65,
      filteredOutCount: 47,
      accounts: [
        {
          serverId: 1,
          serverName: "One.Plex",
          accountId: 1,
          accountName: "One.Night",
          isOwner: true,
          matchedViewerId: 1,
          matchedViewerName: "One.Night",
          plexRelation: "owner",
          plexSources: ["owner"],
        },
        {
          serverId: 1,
          serverName: "One.Plex",
          accountId: 1001,
          accountName: "Kids Home",
          isOwner: false,
          matchedViewerId: 0,
          plexUsername: "kidshome",
          plexRelation: "home",
          plexSources: ["home", "ether_share"],
        },
        {
          serverId: 1,
          serverName: "One.Plex",
          accountId: 1002,
          accountName: "Alice Shared",
          isOwner: false,
          matchedViewerId: 0,
          plexUsername: "alice99",
          plexRelation: "shared",
          plexSources: ["ether_share", "pms_share"],
        },
        {
          serverId: 1,
          serverName: "One.Plex",
          accountId: 1003,
          accountName: "dsabelag",
          isOwner: false,
          matchedViewerId: 0,
          plexUsername: "dsabelag",
          plexRelation: "pending",
          plexSources: ["pending_invite"],
        },
        {
          serverId: 1,
          serverName: "One.Plex",
          accountId: 1004,
          accountName: "Community Pal",
          isOwner: false,
          matchedViewerId: 0,
          plexUsername: "compal",
          plexRelation: "friend",
          plexSources: ["community_friend", "friend"],
        },
        {
          serverId: 1,
          serverName: "One.Plex",
          accountId: 143639010,
          accountName: "Patricson Poliard",
          isOwner: false,
          matchedViewerId: 0,
          plexRelation: "historical",
          plexSources: ["pms_history"],
        },
      ],
    },
  ];

  renderApp(
    "/viewers",
    {},
    extraRoutes((method, path) => {
      if (path === "/viewers/plex-candidates" && method === "GET") {
        return candidateServers;
      }
      return undefined;
    }),
  );

  fireEvent.click(await screen.findByRole("button", { name: /Import from Plex/i }, WAIT));
  const drawer = await screen.findByRole(
    "dialog",
    { name: /Import users from connected Plex servers/i },
    WAIT,
  );

  expect(
    await within(drawer).findByText("Plex.tv matched: 5 (65 in directory)", {}, WAIT),
  ).toBeInTheDocument();
  expect(
    within(drawer).getByText(
      "Showing active Plex accounts with an email (47 inactive or without email hidden)",
    ),
  ).toBeInTheDocument();
  expect(within(drawer).getByText("PMS history only: 1")).toBeInTheDocument();

  const filterGroup = within(drawer).getByRole("group", { name: "Account category" });
  expect(within(filterGroup).getByRole("button", { name: "All (6)" })).toBeInTheDocument();
  expect(within(filterGroup).getByRole("button", { name: "Unmapped (5)" })).toBeInTheDocument();
  expect(
    within(filterGroup).getByRole("button", { name: "Accepted shares (2)" }),
  ).toBeInTheDocument();
  expect(within(filterGroup).getByRole("button", { name: "Home users (1)" })).toBeInTheDocument();
  expect(
    within(filterGroup).getByRole("button", { name: "Pending invites (1)" }),
  ).toBeInTheDocument();
  expect(within(filterGroup).getByRole("button", { name: "Friends (1)" })).toBeInTheDocument();
  expect(
    within(filterGroup).getByRole("button", { name: "Community friends (1)" }),
  ).toBeInTheDocument();
  expect(
    within(filterGroup).getByRole("button", { name: "PMS history only (1)" }),
  ).toBeInTheDocument();

  // Grouped section headers in All view.
  expect(within(drawer).getByText("Server owner & Home users")).toBeInTheDocument();
  expect(within(drawer).getByText("Accepted shares")).toBeInTheDocument();
  expect(within(drawer).getByText("Pending invites")).toBeInTheDocument();
  expect(within(drawer).getByText("Friends")).toBeInTheDocument();
  expect(
    within(drawer).getByText(
      "Historical accounts from this Plex server no longer in your Plex.tv friends or shares",
    ),
  ).toBeInTheDocument();

  // Provenance badges and Plex username display.
  expect(within(drawer).getByText("(@alice99)")).toBeInTheDocument();
  expect(within(drawer).getAllByText("Shared on Ether").length).toBeGreaterThan(0);
  expect(within(drawer).getByText("Shared on One.Plex")).toBeInTheDocument();
  expect(within(drawer).getByText("Pending invite")).toBeInTheDocument();
  expect(within(drawer).getByText("Community friend")).toBeInTheDocument();
  expect(within(drawer).getAllByText("PMS history only").length).toBeGreaterThan(0);

  // Filter by PMS history only.
  fireEvent.click(within(filterGroup).getByRole("button", { name: "PMS history only (1)" }));
  expect(within(drawer).getByText("Patricson Poliard")).toBeInTheDocument();
  expect(within(drawer).queryByText("Alice Shared")).toBeNull();
  expect(within(drawer).queryByText("dsabelag")).toBeNull();

  // Filter by Pending invites.
  fireEvent.click(within(filterGroup).getByRole("button", { name: "Pending invites (1)" }));
  expect(within(drawer).getByText("dsabelag")).toBeInTheDocument();
  expect(within(drawer).queryByText("Patricson Poliard")).toBeNull();

  // Switch back to All and search by plexUsername.
  fireEvent.click(within(filterGroup).getByRole("button", { name: "All (6)" }));
  const searchInput = within(drawer).getByPlaceholderText(
    /Search by account name or Plex username/i,
  );
  fireEvent.change(searchInput, { target: { value: "alice99" } });
  expect(within(drawer).getByText("Alice Shared")).toBeInTheDocument();
  expect(within(drawer).queryByText("Patricson Poliard")).toBeNull();
});
