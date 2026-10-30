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
  it("redirects the old destination and browses pages, filters, views and title details", async () => {
    const { fake } = renderAppWith("/metadata/catalogue", (_method, path, _body, query) => {
      if (path === "/catalog/browse") return { items: [title], total: query.get("q") ? 1 : 107 };
      if (path === `/catalog/title/${title.key}`)
        return {
          ...title,
          summary: "A detailed overview",
          cast: [{ id: 1, name: "An Actor", role: "Detective" }],
        };
    });
    const inspect = await screen.findByRole("button", { name: "Inspect Seven" });
    expect(screen.getByRole("heading", { name: "Catalog", level: 1 })).toBeInTheDocument();
    fireEvent.click(inspect);
    const detail = await screen.findByRole("dialog", { name: "Seven" });
    expect(await within(detail).findByText("A detailed overview")).toBeInTheDocument();
    expect(within(detail).getByRole("heading", { name: "An Actor" })).toBeInTheDocument();
    fireEvent.keyDown(document, { key: "Escape" });
    await waitFor(() =>
      expect(screen.queryByRole("dialog", { name: "Seven" })).not.toBeInTheDocument(),
    );
    fireEvent.click(screen.getByRole("button", { name: "Next" }));
    await screen.findByText("Page 2 of 3");
    expect(calls(fake, "GET", "/catalog/browse").at(-1)?.path).toContain("offset=48");
    fireEvent.click(screen.getByRole("button", { name: "List view" }));
    expect(screen.getByRole("button", { name: "List view" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
    fireEvent.change(screen.getByLabelText("Find a title"), { target: { value: "Seven" } });
    await screen.findByText("1 matching title");
    expect(calls(fake, "GET", "/catalog/browse").at(-1)?.path).toContain("offset=0");
    expect(screen.getByRole("button", { name: "List view" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
    fireEvent.click(screen.getByRole("button", { name: "Filter by genre" }));
    fireEvent.click(await screen.findByRole("menuitemradio", { name: "Drama" }));
    await waitFor(() =>
      expect(calls(fake, "GET", "/catalog/browse").at(-1)?.path).toContain("genre=Drama"),
    );
  });

  it("keeps all catalog settings and saves changes from the new page", async () => {
    const { fake } = renderAppWith("/catalog/settings", () => undefined);
    const language = await screen.findByLabelText("Language");
    expect(screen.getByLabelText("Region")).toBeInTheDocument();
    expect(screen.getByLabelText("Minimum popularity")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Test TMDB key" })).toBeInTheDocument();
    const gate = screen.getByLabelText("Wait for digital or physical release");
    expect(gate).not.toBeChecked();
    fireEvent.click(gate);
    const upcoming = screen.getByLabelText("Show scheduled episodes");
    expect(upcoming).not.toBeChecked();
    fireEvent.click(upcoming);
    fireEvent.change(language, { target: { value: "fr-FR" } });
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/catalog").length).toBe(1));
    expect(calls(fake, "PUT", "/config/catalog")[0]?.body).toMatchObject({
      language: "fr-FR",
      gateMoviesByHomeRelease: true,
      showUpcomingEpisodes: true,
    });
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
    fireEvent.click(screen.getByRole("button", { name: "Clear filters" }));
    await waitFor(() => expect(screen.getByLabelText("Find a title")).toHaveValue(""));
  });
});
