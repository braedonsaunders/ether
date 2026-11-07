import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { renderAppWith } from "./routes";
import { calls } from "./harness";

const title = {
  key: "1000000000000007",
  title: "Seven",
  type: "movie",
  year: 1995,
  genres: ["Drama"],
  rating: 8.6,
};

describe("Catalog workspace", () => {
  it("distinguishes catalog size from the rules result", async () => {
    const { fake } = renderAppWith("/catalog/settings", (_method, path) => {
      if (path === "/catalog/visibility/preview") return { total: 1000, visible: 700, hidden: 300 };
    });
    expect(
      await screen.findByText("700 shown after your rules", {}, { timeout: 15000 }),
    ).toBeInTheDocument();
    expect(screen.getByText("Catalog size before rules")).toBeInTheDocument();
    expect(screen.getByText("1,000 titles")).toBeInTheDocument();
    expect(calls(fake, "POST", "/catalog/visibility/preview").at(-1)?.body).toMatchObject({
      visibilityDefault: "show",
    });
  });

  it("browses pages, filters, views and title details", async () => {
    const { fake } = renderAppWith("/catalog", (_method, path, _body, query) => {
      if (path === "/catalog/browse")
        return {
          items: query.get("q")
            ? [title]
            : [
                title,
                ...Array.from({ length: 47 }, (_, index) => ({
                  ...title,
                  key: String(index),
                  title: `Other film ${index}`,
                })),
              ],
          total: query.get("q") ? 1 : 107,
          hasMore: !query.get("q") && !query.get("after"),
          nextCursor: query.get("q") || query.get("after") ? "" : "page-two",
        };
      if (path === `/catalog/title/${title.key}`)
        return {
          ...title,
          summary: "A detailed overview",
          cast: [{ id: 1, name: "An Actor", role: "Detective" }],
        };
    });
    const inspect = await screen.findByRole(
      "button",
      { name: "Inspect Seven" },
      { timeout: 15000 },
    );
    expect(screen.getByRole("heading", { name: "Catalog", level: 1 })).toBeInTheDocument();
    fireEvent.click(inspect);
    const detail = await screen.findByRole("dialog", { name: "Seven" });
    expect(await within(detail).findByText("A detailed overview")).toBeInTheDocument();
    expect(within(detail).getByRole("heading", { name: "An Actor" })).toBeInTheDocument();
    fireEvent.keyDown(document, { key: "Escape" });
    await waitFor(() =>
      expect(screen.queryByRole("dialog", { name: "Seven" })).not.toBeInTheDocument(),
    );
    fireEvent.click(screen.getByRole("button", { name: "Load more" }));
    await waitFor(() =>
      expect(
        calls(fake, "GET", "/catalog/browse")
          .filter((c) => !c.path.includes("countOnly"))
          .at(-1)?.path,
      ).toContain("after=page-two"),
    );
    expect(
      calls(fake, "GET", "/catalog/browse")
        .filter((c) => !c.path.includes("countOnly"))
        .at(-1)?.path,
    ).toContain("after=page-two");
    fireEvent.click(screen.getByRole("button", { name: "List view" }));
    expect(screen.getByRole("button", { name: "List view" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
    fireEvent.change(screen.getByLabelText("Find a title"), { target: { value: "Seven" } });
    await screen.findByText("1 matching title");
    expect(
      calls(fake, "GET", "/catalog/browse")
        .filter((c) => !c.path.includes("countOnly"))
        .at(-1)?.path,
    ).toContain("cursor=1");
    expect(screen.getByRole("button", { name: "List view" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
    fireEvent.click(screen.getByRole("button", { name: "Filter" }));
    fireEvent.click(screen.getByRole("button", { name: "Genre" }));
    fireEvent.change(screen.getByLabelText("Value"), { target: { value: "Drama" } });
    fireEvent.click(screen.getByRole("button", { name: "Apply" }));
    await waitFor(() => {
      const url = new URL(
        calls(fake, "GET", "/catalog/browse")
          .filter((c) => !c.path.includes("countOnly"))
          .at(-1)?.path || "",
        "http://localhost",
      );
      expect(url.searchParams.get("filter")).toContain('"genres":["Drama"]');
    });
  });

  it("uses canonical facet keys for language and decade suggestions", async () => {
    const { fake } = renderAppWith("/catalog", (_method, path, _body, query) => {
      if (path === "/catalog/browse") return { items: [title], total: 1 };
      if (path === "/catalog/facets")
        return {
          pending: false,
          values:
            query.get("facet") === "language"
              ? [{ key: "en", title: "English", count: 1 }]
              : [{ key: "1980", title: "1980s", count: 1 }],
        };
    });
    await screen.findByRole("button", { name: "Inspect Seven" });
    for (const [field, label] of [
      ["Original language", "English"],
      ["Decade", "1980s"],
    ] as const) {
      fireEvent.click(screen.getByRole("button", { name: "Filter" }));
      fireEvent.click(screen.getByRole("button", { name: field }));
      fireEvent.click(await screen.findByRole("button", { name: new RegExp(label!) }));
      fireEvent.click(screen.getByRole("button", { name: "Apply" }));
    }
    await waitFor(() => {
      const url = new URL(
        calls(fake, "GET", "/catalog/browse")
          .filter((c) => !c.path.includes("countOnly"))
          .at(-1)?.path || "",
        "http://localhost",
      );
      expect(url.searchParams.get("filter")).toContain('"languages":["en"]');
      expect(url.searchParams.get("filter")).toContain('"yearFrom":1980');
      expect(url.searchParams.get("filter")).toContain('"yearTo":1989');
    });
  });

  it("restarts a failed cursor page without preserving a broken boundary", async () => {
    const { fake } = renderAppWith("/catalog?page=999999", (_method, path, _body, query) => {
      if (path !== "/catalog/browse" || query.has("countOnly")) return undefined;
      if (query.has("after"))
        return new Response(JSON.stringify({ error: { message: "Restart browsing" } }), {
          status: 400,
        });
      return {
        items: Array.from({ length: 48 }, (_, i) => ({
          ...title,
          key: String(i),
          title: `Film ${i}`,
        })),
        total: 107,
        hasMore: true,
        nextCursor: "expired",
      };
    });
    await screen.findByRole("button", { name: "Inspect Film 0" });
    expect(
      calls(fake, "GET", "/catalog/browse")
        .filter((c) => !c.path.includes("countOnly"))
        .at(-1)?.path,
    ).not.toContain("offset=");
    fireEvent.click(screen.getByRole("button", { name: "Load more" }));
    fireEvent.click(await screen.findByRole("button", { name: "Try again" }));
    await screen.findByRole("button", { name: "Inspect Film 0" });
    const latest = calls(fake, "GET", "/catalog/browse")
      .filter((c) => !c.path.includes("countOnly"))
      .at(-1)?.path;
    expect(latest).not.toContain("after=");
    expect(latest).not.toContain("offset=");
  });

  it("reloads visible-card membership after adding a selected set to the watchlist", async () => {
    const { fake } = renderAppWith("/catalog", (_method, path) => {
      if (path === "/catalog/browse") return { items: [title], total: 1 };
    });
    await screen.findByRole("button", { name: "Inspect Seven" });
    fireEvent.click(screen.getByRole("button", { name: "Select loaded titles" }));
    fireEvent.click(screen.getAllByRole("button", { name: "Add to Watchlist" })[0]!);
    await screen.findByRole("button", { name: "Remove from Watchlist" });
    expect(calls(fake, "PUT", `/discovery/users/1/catalog/${title.key}`)).toHaveLength(1);
  });

  it("sends effective primary and secondary sort directions and reverses title on the first click", async () => {
    const { fake } = renderAppWith("/catalog?sort=title&secondarySort=rating", (_method, path) =>
      path === "/catalog/browse" ? { items: [title], total: 1 } : undefined,
    );
    await screen.findByRole("button", { name: "Inspect Seven" });
    const last = () =>
      new URL(
        calls(fake, "GET", "/catalog/browse")
          .filter((c) => !c.path.includes("countOnly"))
          .at(-1)!.path,
        "http://localhost",
      ).searchParams;
    expect(last().get("desc")).toBe("0");
    expect(last().get("secondaryDesc")).toBe("1");
    fireEvent.click(screen.getByRole("button", { name: "Reverse sort direction" }));
    await waitFor(() => expect(last().get("desc")).toBe("1"));
    fireEvent.click(
      screen.getByRole("button", { name: "Secondary sort · Reverse sort direction" }),
    );
    await waitFor(() => expect(last().get("secondaryDesc")).toBe("0"));
    fireEvent.click(screen.getByRole("button", { name: "Sort titles" }));
    expect(await screen.findByRole("menuitemradio", { name: "Year" })).toBeInTheDocument();
    expect(screen.getByRole("menuitemradio", { name: "Runtime" })).toBeInTheDocument();
  });
  it("preserves a shared secondary direction override and supports runtime and year sorts", async () => {
    const { fake } = renderAppWith(
      "/catalog?sort=duration&secondarySort=rating&secondaryDesc=0",
      (_method, path) => (path === "/catalog/browse" ? { items: [title], total: 1 } : undefined),
    );
    await screen.findByRole("button", { name: "Inspect Seven" });
    const last = () =>
      new URL(
        calls(fake, "GET", "/catalog/browse")
          .filter((c) => !c.path.includes("countOnly"))
          .at(-1)!.path,
        "http://localhost",
      ).searchParams;
    expect(last().get("sort")).toBe("duration");
    expect(last().get("desc")).toBe("0");
    expect(last().get("secondaryDesc")).toBe("0");
    fireEvent.click(screen.getByRole("button", { name: "Sort titles" }));
    fireEvent.click(await screen.findByRole("menuitemradio", { name: "Year" }));
    await waitFor(() => expect(last().get("sort")).toBe("year"));
    expect(last().get("desc")).toBe("1");
    expect(last().get("secondaryDesc")).toBe("0");
  });

  it("keeps all catalog settings and saves changes from the new page", async () => {
    const { fake } = renderAppWith("/catalog/settings", () => undefined);
    const language = await screen.findByLabelText("Language");
    expect(screen.queryByText("Catalog healthy")).not.toBeInTheDocument();
    expect(screen.getByRole("region", { name: "Your catalog" })).toBeInTheDocument();
    expect(screen.queryByText(/Your saved release-date requirement/)).not.toBeInTheDocument();
    expect(screen.getByLabelText("Region")).toBeInTheDocument();
    expect(screen.queryByLabelText("Minimum popularity")).not.toBeInTheDocument();
    expect(screen.getByRole("radio", { name: "Some" })).toBeChecked();
    fireEvent.click(screen.getByRole("button", { name: "More options: What's browsable" }));
    expect(screen.getByLabelText("Minimum popularity")).toBeInTheDocument();
    expect(screen.queryByText("TMDB connected")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Your TMDB API key")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("radio", { name: /On digital release/ }));
    const upcoming = screen.getByLabelText("Show upcoming episodes");
    expect(upcoming).not.toBeChecked();
    fireEvent.click(upcoming);
    const movies = screen.getByLabelText("Show upcoming movies");
    expect(movies).not.toBeChecked();
    fireEvent.click(movies);
    fireEvent.click(language);
    fireEvent.click(await screen.findByRole("option", { name: "French (France)" }));
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/catalog").length).toBe(1));
    expect(calls(fake, "PUT", "/config/catalog")[0]?.body).toMatchObject({
      language: "fr-FR",
      showUpcomingEpisodes: true,
      showUpcomingMovies: true,
    });
    expect(screen.getByRole("radio", { name: /On digital release/ })).toBeVisible();
  });

  it("filters the catalog to titles waiting for a release", async () => {
    const { fake } = renderAppWith("/catalog?waiting=1", (_method, path) => {
      if (path === "/catalog/browse") return { items: [title], total: 1 };
    });
    expect(await screen.findByRole("heading", { name: "Waiting for a release" })).toBeVisible();
    expect(
      screen.getByRole("button", { name: "Remove Waiting for a release filter" }),
    ).toBeVisible();
    await waitFor(() =>
      expect(
        calls(fake, "GET", "/catalog/browse")
          .filter((c) => !c.path.includes("countOnly"))
          .at(-1)?.path,
      ).toContain("waiting=1"),
    );
    fireEvent.click(screen.getByRole("button", { name: "Remove Waiting for a release filter" }));
    await waitFor(() =>
      expect(
        calls(fake, "GET", "/catalog/browse")
          .filter((c) => !c.path.includes("countOnly"))
          .at(-1)?.path,
      ).not.toContain("waiting=1"),
    );
  });

  it("shows a one-line notice linking to /services/tmdb when no TMDB key is configured", async () => {
    renderAppWith("/catalog", (_method, path) => {
      if (path === "/catalog/browse") return { items: [], total: 0 };
    });
    const link = await screen.findByRole("link", { name: "Add a TMDB key" });
    expect(link).toHaveAttribute("href", "/services/tmdb");
  });

  it("shows errors with retry and provides an empty-result reset", async () => {
    let fail = true;
    renderAppWith("/catalog?q=missing", (_method, path) => {
      if (path === "/catalog/browse")
        return fail
          ? new Response(JSON.stringify({ error: { message: "Catalog offline" } }), { status: 503 })
          : { items: [], total: 0 };
    });
    const retry = await screen.findByRole("button", { name: "Try again" });
    fail = false;
    fireEvent.click(retry);
    expect(await screen.findByText("No matching titles")).toBeInTheDocument();
    fireEvent.click(screen.getAllByRole("button", { name: "Clear filters" })[0]!);
    await waitFor(() => expect(screen.getByLabelText("Find a title")).toHaveValue(""));
  });
});
