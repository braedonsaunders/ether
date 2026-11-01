import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { CollectionSpec, Rule } from "../lib/contract";
import { calls, renderApp } from "./harness";
import { renderAppWith } from "./routes";

const WAIT = { timeout: 4000 };

function previewBodies(fake: Parameters<typeof calls>[0]): CollectionSpec[] {
  return calls(fake, "POST", "/collections/preview").map((c) => c.body as CollectionSpec);
}
function hasRule(spec: CollectionSpec, pred: (r: Rule) => boolean) {
  return spec.rules.some(pred);
}

describe("Collection Studio editor", () => {
  it("saves an explicit upcoming window without changing the default", async () => {
    const { fake } = renderApp("/collections/new?draft=test-upcoming");
    const name = await screen.findByLabelText("Collection name", {}, { timeout: 10000 });
    fireEvent.change(name, { target: { value: "Look Ahead" } });
    expect(screen.queryByLabelText("Look ahead (days)")).toBeNull();
    fireEvent.click(screen.getByRole("radio", { name: "Upcoming" }));
    fireEvent.change(screen.getByLabelText("Look ahead (days)"), { target: { value: "45" } });
    await waitFor(
      () =>
        expect(
          previewBodies(fake).some((s) => s.releaseMode === "upcoming" && s.lookaheadDays === 45),
        ).toBe(true),
      WAIT,
    );
    fireEvent.click(screen.getByRole("button", { name: /^Save$/ }));
    await waitFor(
      () =>
        expect(
          calls(fake, "POST", "/collections").some(
            (c) => (c.body as CollectionSpec).lookaheadDays === 45,
          ),
        ).toBe(true),
      WAIT,
    );
  });
  it("creates a collection in a drawer over the gallery", async () => {
    renderApp("/collections/new?draft=test-drawer");
    const drawer = await screen.findByRole("dialog", { name: "New collection" }, WAIT);
    expect(await within(drawer).findByLabelText("Collection name", {}, WAIT)).toBeInTheDocument();
    fireEvent.click(within(drawer).getByRole("button", { name: "Close" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull(), WAIT);
    expect(await screen.findByRole("heading", { name: "Collections" }, WAIT)).toBeInTheDocument();
  });

  it("edits rules with live, debounced previews, undo, and saves", async () => {
    const { fake } = renderApp("/collections/new?draft=test-1");
    const name = await screen.findByLabelText("Collection name", {}, WAIT);
    fireEvent.change(name, { target: { value: "Late-night thrillers" } });

    // The initial preview arrives without any rules.
    await waitFor(() => expect(previewBodies(fake).length).toBeGreaterThan(0), WAIT);
    expect(
      await within(await screen.findByRole("region", { name: "Live preview" }, WAIT)).findByText(
        /\d+ titles/,
        {},
        WAIT,
      ),
    ).toBeInTheDocument();

    // Add a rating rule from the field menu.
    fireEvent.click(screen.getByRole("button", { name: /Add rule/ }));
    fireEvent.click(await screen.findByRole("menuitem", { name: "Rating" }));
    const value = await screen.findByLabelText("Rule 1 value");
    await waitFor(
      () =>
        expect(
          previewBodies(fake).some((s) =>
            hasRule(s, (r) => r.field === "rating" && r.op === "gte" && r.value === 7),
          ),
        ).toBe(true),
      WAIT,
    );

    // Typing a new value re-runs the preview once the edits settle.
    const before = previewBodies(fake).length;
    fireEvent.change(value, { target: { value: "8.5" } });
    await waitFor(
      () =>
        expect(
          previewBodies(fake).some((s) =>
            hasRule(s, (r) => r.field === "rating" && r.value === 8.5),
          ),
        ).toBe(true),
      WAIT,
    );
    expect(previewBodies(fake).length).toBe(before + 1);
    expect(screen.getByText("Rating is at least 8.5")).toBeInTheDocument();

    // Undo restores the previous value.
    fireEvent.click(screen.getByRole("button", { name: "Undo" }));
    await waitFor(() => expect(screen.getByLabelText("Rule 1 value")).toHaveValue(7));

    // Save creates the collection with the edited spec, unpublished.
    fireEvent.click(screen.getByRole("button", { name: /^Save$/ }));
    await waitFor(
      () =>
        expect(
          calls(fake, "POST", "/collections").filter((c) => c.path === "/collections"),
        ).toHaveLength(1),
      WAIT,
    );
    const created = calls(fake, "POST", "/collections").find((c) => c.path === "/collections")!
      .body as CollectionSpec;
    expect(created.name).toBe("Late-night thrillers");
    expect(created.enabled).toBe(false);
    expect(created.rules).toEqual([{ field: "rating", op: "gte", value: 7 }]);
    // The editor now shows the saved collection.
    await waitFor(
      () => expect(calls(fake, "GET", "/collections/c").length).toBeGreaterThan(0),
      WAIT,
    );
    expect(
      await within(
        await screen.findByRole("region", { name: "Collection Studio" }, WAIT),
      ).findByText("Draft", {}, WAIT),
    ).toBeInTheDocument();
  });

  it("sends each air-date sort and direction to the live preview", async () => {
    const { fake } = renderApp("/collections/new?draft=air-sort");
    await screen.findByLabelText("Collection name", {}, WAIT);
    for (const [label, sort] of [
      ["Latest aired episode", "latestEpisode"],
      ["Latest season premiere", "latestSeasonPremiere"],
    ] as const) {
      fireEvent.click(screen.getByRole("button", { name: /Sort by/ }));
      fireEvent.click(await screen.findByRole("option", { name: new RegExp(label) }));
      for (const [direction, desc] of [
        ["Highest", true],
        ["Lowest", false],
      ] as const) {
        fireEvent.click(screen.getByRole("radio", { name: direction }));
        await waitFor(
          () =>
            expect(previewBodies(fake).some((s) => s.sort === sort && s.sortDesc === desc)).toBe(
              true,
            ),
          WAIT,
        );
      }
    }
  });

  it("refuses to save an incomplete collection and says why", async () => {
    const { fake } = renderApp("/collections/new?draft=test-2");
    await screen.findByLabelText("Collection name", {}, WAIT);
    fireEvent.click(screen.getByRole("button", { name: /^Publish$/ }));
    expect((await screen.findAllByText("Give the collection a name.")).length).toBeGreaterThan(0);
    expect(
      calls(fake, "POST", "/collections").filter((c) => c.path === "/collections"),
    ).toHaveLength(0);
  });

  it("keeps an unsaved draft on the device and restores it", async () => {
    const first = renderApp("/collections/new?draft=test-3");
    fireEvent.change(await screen.findByLabelText("Collection name", {}, WAIT), {
      target: { value: "Saturday matinee" },
    });
    await waitFor(
      () =>
        expect(localStorage.getItem("ether.draft.collection.test-3")).toContain("Saturday matinee"),
      WAIT,
    );
    first.unmount();
    renderApp("/collections/new?draft=test-3");
    expect(await screen.findByDisplayValue("Saturday matinee", {}, WAIT)).toBeInTheDocument();
    expect(screen.getByText(/Restored unsaved changes/)).toBeInTheDocument();
  });
});

describe("Collection Studio gallery", () => {
  it("keeps the library mounted and its selection unchanged while opening a new drawer", async () => {
    const { fake } = renderApp("/collections?view=posters&collection=c3");
    const library = await screen.findByRole("complementary", { name: "Collection library" }, WAIT);
    const gallery = await screen.findByRole("region", { name: "Collection posters" }, WAIT);
    const count = calls(fake, "GET", "/collections").filter(
      (c) => c.path === "/collections",
    ).length;
    fireEvent.click(screen.getByRole("button", { name: "New collection" }));
    const drawer = await screen.findByRole("dialog", { name: "New collection" }, WAIT);
    await within(drawer).findByLabelText("Collection name", {}, WAIT);
    expect(library.isConnected).toBe(true);
    expect(gallery.isConnected).toBe(true);
    expect(library.closest("[inert]")).not.toBeNull();
    expect(
      calls(fake, "GET", "/collections").filter((c) => c.path === "/collections"),
    ).toHaveLength(count);
    fireEvent.click(within(drawer).getByRole("button", { name: "Close" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull(), WAIT);
    expect(screen.getByRole("region", { name: "Collection posters" })).toBe(gallery);
    expect(screen.getByRole("complementary", { name: "Collection library" })).toBe(library);
    expect(screen.getByRole("button", { name: "Preview Family movie night" })).toHaveAttribute(
      "aria-current",
      "true",
    );
  });

  it("previews a selected collection in place and publishes it", async () => {
    const { fake } = renderApp("/collections?collection=c3");
    expect(
      await screen.findByRole("heading", { name: "Family movie night" }, WAIT),
    ).toBeInTheDocument();
    expect(screen.queryByRole("dialog")).toBeNull();
    const row = screen.getByRole("button", { name: "Preview Family movie night" });
    expect(row).toHaveAttribute("aria-current", "true");
    await waitFor(
      () => expect(previewBodies(fake).some((s) => s.name === "Family movie night")).toBe(true),
      WAIT,
    );
    fireEvent.click(screen.getByRole("button", { name: "Publish" }));
    await waitFor(() => expect(calls(fake, "PUT", "/collections/c3")).toHaveLength(1), WAIT);
    expect(await screen.findByRole("button", { name: "Unpublish" }, WAIT)).toBeInTheDocument();
    expect(within(row).getByText("Published")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Preview New to Ether" }));
    expect(await screen.findByRole("heading", { name: "New to Ether" }, WAIT)).toBeInTheDocument();
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("filters the library and recovers from an empty search", async () => {
    renderApp("/collections");
    await screen.findByRole("heading", { name: "Edge of your seat" }, WAIT);
    fireEvent.click(screen.getByRole("button", { name: "Collection status" }));
    fireEvent.click(await screen.findByRole("menuitemradio", { name: "Drafts" }));
    expect(
      await screen.findByRole("heading", { name: "Family movie night" }, WAIT),
    ).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Preview Edge of your seat" })).toBeNull();
    fireEvent.change(screen.getByRole("searchbox", { name: "Search collections" }), {
      target: { value: "no such collection" },
    });
    expect(await screen.findByRole("heading", { name: "No matches" }, WAIT)).toBeInTheDocument();
    fireEvent.change(screen.getByRole("searchbox", { name: "Search collections" }), {
      target: { value: "" },
    });
    expect(
      await screen.findByRole("heading", { name: "Family movie night" }, WAIT),
    ).toBeInTheDocument();
  });

  it("customises built-in artwork while preserving the managed definition", async () => {
    const { fake } = renderApp("/collections?tab=builtin");
    await screen.findByRole("heading", { name: "Recently added" }, WAIT);
    fireEvent.click(screen.getByRole("button", { name: "Customise collection" }));
    const drawer = await screen.findByRole("dialog", { name: "Edit collection" }, WAIT);
    expect(await within(drawer).findByLabelText("Collection name", {}, WAIT)).toBeDisabled();
    fireEvent.click(await within(drawer).findByRole("radio", { name: "Custom image" }, WAIT));
    fireEvent.change(within(drawer).getByLabelText("Custom image URL"), {
      target: { value: "https://example.com/collection.jpg" },
    });
    fireEvent.click(within(drawer).getByRole("button", { name: /^Save$/ }));
    await waitFor(
      () => expect(calls(fake, "PUT", "/collections/builtin-recent")).toHaveLength(1),
      WAIT,
    );
    const saved = fake.state.collections.find((c) => c.id === "builtin-recent")!;
    expect(saved.artwork).toMatchObject({
      mode: "custom",
      url: "https://example.com/collection.jpg",
    });
    expect(saved.name).toBe("Recently added");
    expect(saved.builtIn).toBe(true);
  });

  it("browses native collection posters and opens a selected preview", async () => {
    renderApp("/collections?view=posters");
    const gallery = await screen.findByRole("region", { name: "Collection posters" }, WAIT);
    expect(within(gallery).getAllByRole("button", { name: /Open .+ preview/ })).toHaveLength(4);
    fireEvent.click(
      within(gallery).getByRole("button", { name: "Open Family movie night preview" }),
    );
    expect(
      await screen.findByRole("heading", { name: "Family movie night" }, WAIT),
    ).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Show collection preview" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
  });

  it("saves a chosen title poster as the original TMDB image path", async () => {
    const { fake } = renderAppWith("/collections/c1?section=artwork", (method, path) => {
      if (method === "POST" && path === "/collections/preview")
        return {
          total: 1,
          warnings: [],
          tookMs: 1,
          items: [
            {
              key: "m603",
              type: "movie",
              title: "The Matrix",
              year: 1999,
              poster: "/ether/api/image?path=%2Fmatrix.jpg&w=342",
            },
          ],
        };
    });
    const drawer = await screen.findByRole("dialog", { name: "Edit collection" }, WAIT);
    fireEvent.click(
      await within(drawer).findByRole("button", { name: "Use The Matrix poster" }, WAIT),
    );
    expect(within(drawer).getByLabelText("Custom image URL")).toHaveValue("/matrix.jpg");
    fireEvent.click(within(drawer).getByRole("button", { name: /^Save$/ }));
    await waitFor(() => expect(calls(fake, "PUT", "/collections/c1")).toHaveLength(1), WAIT);
    expect(
      (calls(fake, "PUT", "/collections/c1")[0]!.body as CollectionSpec).artwork,
    ).toMatchObject({ mode: "custom", url: "/matrix.jpg" });
  });

  it("lists collections and starts a new one from a template", async () => {
    renderApp("/collections");
    expect(
      await screen.findByRole("heading", { name: "Edge of your seat" }, WAIT),
    ).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Collection views" }));
    fireEvent.click(screen.getByRole("menuitemradio", { name: /Templates/ }));
    fireEvent.click(
      await screen.findByRole("button", { name: "Preview template Hidden gems" }, WAIT),
    );
    const card = await screen.findByRole("button", { name: "Use template Hidden gems" }, WAIT);
    fireEvent.click(card);
    expect(await screen.findByDisplayValue("Hidden gems", {}, WAIT)).toBeInTheDocument();
    expect(
      within(await screen.findByRole("dialog", { name: "New collection" }, WAIT)).getByText(
        "Rating is at least 7.5",
      ),
    ).toBeInTheDocument();
  });

  it("drafts a collection from a plain-English description", async () => {
    const { fake } = renderApp("/collections");
    await screen.findByRole("heading", { name: "Edge of your seat" }, WAIT);
    fireEvent.click(screen.getByRole("button", { name: /Describe it/ }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.change(within(dialog).getByLabelText("Describe the collection"), {
      target: { value: "Acclaimed horror from the 80s" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: /Draft it/ }));
    expect(await within(dialog).findByText(/Genre is any of Horror/, {}, WAIT)).toBeInTheDocument();
    expect(calls(fake, "POST", "/collections/generate")[0]?.body).toEqual({
      prompt: "Acclaimed horror from the 80s",
      mediaType: "movie",
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Open as draft" }));
    expect(await screen.findByLabelText("Collection name", {}, WAIT)).toHaveValue(
      "Acclaimed horror from the 80s",
    );
  });

  it("explains how to enable Describe it when no language model is set up", async () => {
    renderApp("/collections", { llm: false });
    await screen.findByRole("heading", { name: "Edge of your seat" }, WAIT);
    fireEvent.click(screen.getByRole("button", { name: /Describe it/ }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.change(within(dialog).getByLabelText("Describe the collection"), {
      target: { value: "Cosy mysteries" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: /Draft it/ }));
    expect(
      await within(dialog).findByText("Describe it needs a language model", {}, WAIT),
    ).toBeInTheDocument();
  });
});
