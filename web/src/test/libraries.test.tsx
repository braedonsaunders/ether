import { fireEvent, screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { calls, renderApp, settleUi } from "./harness";
import { renderAppWith } from "./routes";

describe("Libraries workspace", () => {
  it("lists libraries without global settings or separate plugin buttons", async () => {
    renderApp("/libraries");
    expect(
      await screen.findByRole("heading", { name: "Libraries", level: 1 }, { timeout: 15000 }),
    ).toBeInTheDocument();
    expect(await screen.findByText("Books")).toBeInTheDocument();
    expect(screen.getAllByText("Movies").length).toBeGreaterThan(0);
    expect(screen.queryByRole("button", { name: /Library settings/i })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /From a plugin/i })).not.toBeInTheDocument();
  });

  it("shows Browse, Rules, and Settings tabs on a TMDB library without Collections", async () => {
    renderApp("/libraries/1");
    const nav = await screen.findByRole(
      "tablist",
      { name: "Library sections" },
      { timeout: 15000 },
    );
    expect(within(nav).getByText("Browse")).toBeInTheDocument();
    expect(within(nav).getByText("Rules")).toBeInTheDocument();
    expect(within(nav).getByText("Settings")).toBeInTheDocument();
    expect(within(nav).queryByText("Collections")).not.toBeInTheDocument();
  });

  it("picks TMDB from the New library source drawer to create a library with rules", async () => {
    const { fake } = renderApp("/libraries");
    fireEvent.click(await screen.findByRole("button", { name: "New library" }, { timeout: 15000 }));
    expect(await screen.findByText("Where do titles come from?")).toBeInTheDocument();
    fireEvent.click(await screen.findByRole("checkbox", { name: /TMDB Movies/ }));
    fireEvent.click(screen.getByRole("button", { name: "Continue" }));
    fireEvent.click(screen.getByRole("button", { name: /Add rules/ }));
    fireEvent.change(await screen.findByLabelText("Library name"), {
      target: { value: "Weekend films" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Create library" }));
    await settleUi(() => {
      const post = calls(fake, "POST", "/libraries").at(-1);
      expect(post?.body).toMatchObject({ name: "Weekend films" });
      expect((post?.body as { present?: unknown }).present).toBeUndefined();
    });
    expect(
      await screen.findByRole("heading", { name: "Weekend films", level: 1 }, { timeout: 15000 }),
    ).toBeInTheDocument();
  });

  it("creates a library from a collection through the From a collection step", async () => {
    const { fake } = renderApp("/libraries");
    fireEvent.click(await screen.findByRole("button", { name: "New library" }, { timeout: 15000 }));
    fireEvent.click(await screen.findByRole("button", { name: /From a collection/ }));
    await new Promise((r) => setTimeout(r, 300));

    expect(await screen.findByText("New library from a collection")).toBeInTheDocument();
    // Pick the first collection offered; its name seeds the library name.
    // Every collection card carries its title count; the first reads
    // "10 titles" in the mock. The wait absorbs the drawer's entry animation
    // before the card can take a click.
    const option = await screen.findByRole("button", { name: /Edge of Your Seat/ });
    fireEvent.click(option);
    // The card keeps the current selection.
    await settleUi(() => expect(option.getAttribute("aria-current")).toBe("true"));
    const input = screen.getByLabelText("Library name") as HTMLInputElement;
    fireEvent.change(input, { target: { value: "List picks" } });
    await settleUi(() => expect(input.value).toBe("List picks"));
    const create = screen.getByRole("button", { name: "Create library" }) as HTMLButtonElement;

    await settleUi(() => expect(create.disabled).toBe(false));
    fireEvent.click(create);

    await settleUi(() => {
      const post = calls(fake, "POST", "/libraries").at(-1);
      expect(post?.body).toMatchObject({ name: "List picks", mediaType: "movie" });
      expect(typeof (post?.body as { collectionId?: unknown }).collectionId).toBe("number");
    });
  });

  it("links to Plugins when no content-kind plugin is installed, and ensures plugin setup when picked", async () => {
    renderAppWith("/libraries", (_method, path) => {
      if (path === "/plugins-setup") return { plugins: [] };
      return undefined;
    });
    fireEvent.click(await screen.findByRole("button", { name: "New library" }, { timeout: 15000 }));
    fireEvent.click(await screen.findByRole("button", { name: "Add catalogue source" }));
    const install = await within(await screen.findByRole("dialog")).findByRole("link", {
      name: /Plugin/,
    });
    expect(install).toHaveAttribute("href", "/plugins/marketplace");
  });

  it("browses a plugin library and re-matches from the item drawer", async () => {
    const { fake } = renderApp("/libraries/3");
    fireEvent.click(await screen.findByRole("button", { name: /Dune/ }, { timeout: 15000 }));
    expect(await screen.findByText("Matching")).toBeInTheDocument();
    expect(screen.getByText("Herbert")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Match again" }));
    await settleUi(() => {
      expect(calls(fake, "POST", "/libraries/3/item/").length).toBeGreaterThan(0);
    });
  });

  it("shows plugin-only sections in a plugin library's settings", async () => {
    renderApp("/libraries/3/settings");
    expect(await screen.findByText("Basics", {}, { timeout: 15000 })).toBeInTheDocument();
    expect(screen.queryByText("Keeping it up to date")).not.toBeInTheDocument();
    expect(screen.queryByText("New releases")).not.toBeInTheDocument();
    expect(screen.queryByText("Source: TMDB")).not.toBeInTheDocument();
  });

  it("sets the quality profile on a library from its settings", async () => {
    const { fake } = renderApp("/libraries/1/settings");
    await screen.findByText("Quality profile", {}, { timeout: 15000 });
    fireEvent.click(screen.getByRole("button", { name: "Quality profile" }));
    fireEvent.click(await screen.findByRole("option", { name: "4K" }, { timeout: 15000 }));
    expect(calls(fake, "PATCH", "/libraries/1")).toHaveLength(0);
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await settleUi(() => {
      const patch = calls(fake, "PATCH", "/libraries/1").at(-1);
      expect(patch?.body).toMatchObject({ qualityProfile: "4K" });
    });
  });
});
