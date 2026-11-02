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
  maxContentRating: "",
  blockedGenres: [],
  blockedKeywords: [],
  remoteDisabled: false,
  maxBitrateKbps: 0,
  streamLimit: 0,
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
    expect(await screen.findByText("What Ether has learned", {}, WAIT)).toBeInTheDocument();
    expect(await screen.findByText("Up next", {}, WAIT)).toBeInTheDocument();
    expect(await screen.findByText("Pick 1", {}, WAIT)).toBeInTheDocument();
    expect(await screen.findByText("Pick 2", {}, WAIT)).toBeInTheDocument();
  });

  it("corrects taste from the genre chart and saves overrides", async () => {
    const { fake } = renderApp("/viewers/2");
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
    const { fake } = renderApp("/viewers/1");
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

  it("sets a viewer's stream limit and rating cap", async () => {
    const { fake } = renderApp("/viewers/4/access");
    const limit = await screen.findByLabelText("Stream limit", {}, WAIT);
    fireEvent.change(limit, { target: { value: "1" } });
    fireEvent.click(await screen.findByRole("button", { name: /Save changes/ }));
    await waitFor(() => expect(calls(fake, "PATCH", "/users/4")).toHaveLength(1), WAIT);
    expect(calls(fake, "PATCH", "/users/4")[0]!.body).toMatchObject({ streamLimit: 1 });
    // Only the part that changed is written.
    expect(calls(fake, "PUT", "/safety/policies/4")).toHaveLength(0);
  });

  it("saves a viewer's parental controls from their Access tab", async () => {
    const { fake } = renderApp(
      "/viewers/3/access",
      {},
      extraRoutes((method, path) => {
        if (method === "GET" && path === "/safety/policies/3") return POLICY;
        if (method === "GET" && path === "/safety/policies/presets/kids?age=family")
          return { maxContentRating: "PG", blockedKeywords: ["pornography"] };
        if (method === "PUT" && path === "/safety/policies/3") return { ok: true };
        return undefined;
      }),
    );
    fireEvent.click(await screen.findByRole("button", { name: "Family" }, WAIT));
    await waitFor(() =>
      expect(screen.getByLabelText("Blocked keywords")).toHaveValue("pornography"),
    );
    fireEvent.change(screen.getByLabelText("Blocked genres"), {
      target: { value: "Horror, Thriller" },
    });
    fireEvent.click(screen.getByLabelText("Local network only"));
    fireEvent.change(screen.getByLabelText("Concurrent streams"), { target: { value: "2" } });
    fireEvent.click(
      within(screen.getByRole("group", { name: "Allowed hours" })).getByRole("button", {
        name: "20:00",
      }),
    );
    fireEvent.click(screen.getByRole("button", { name: /Save changes/ }));
    await waitFor(() => expect(calls(fake, "PUT", "/safety/policies/3")).toHaveLength(1), WAIT);
    expect(calls(fake, "PUT", "/safety/policies/3")[0]!.body).toMatchObject({
      maxContentRating: "PG",
      blockedGenres: ["Horror", "Thriller"],
      blockedKeywords: ["pornography"],
      remoteDisabled: true,
      streamLimit: 2,
      allowedHours: [20],
    });
    // The user record (stream limit, rating cap) was untouched.
    expect(calls(fake, "PATCH", "/users/3")).toHaveLength(0);
    await waitFor(() => expect(screen.queryByText("You have unsaved changes")).toBeNull());
  });

  it("shows read-only plex.tv share defaults on Access", async () => {
    renderApp("/viewers/3/access", {}, withPlexDefaults);
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
  await screen.findByText("Sam Rivera", {}, WAIT);
  expect(screen.getByText("Imported history", { selector: "span" })).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: /^Active \(/ }));
  expect(screen.queryByText("Sam Rivera")).not.toBeInTheDocument();
  expect(screen.getByText("The Kids")).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: /^All \(/ }));
  expect(screen.getByText("Sam Rivera")).toBeInTheDocument();
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
      ...server.state.config.frontends,
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
  fireEvent.click(within(drawer).getByRole("button", { name: "Create viewer" }));
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

it("hides server-only tabs for a viewer kept for imported history", async () => {
  renderApp("/viewers/2/watchlist", {}, (server) => {
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
});
