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

  it("picks TMDB from the New library source drawer to create a saved-filter library", async () => {
    const { fake } = renderApp("/libraries");
    fireEvent.click(await screen.findByRole("button", { name: "New library" }, { timeout: 15000 }));
    expect(await screen.findByText("Where do titles come from?")).toBeInTheDocument();
    expect(
      await screen.findByText("From Demo books · EPUB and audiobook library"),
    ).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: /Movies or TV from TMDB/ }));
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

  it("links to Plugins when no content-kind plugin is installed, and ensures plugin setup when picked", async () => {
    renderAppWith("/libraries", (_method, path) => {
      if (path === "/plugins-setup") return { plugins: [] };
      return undefined;
    });
    fireEvent.click(await screen.findByRole("button", { name: "New library" }, { timeout: 15000 }));
    const install = await screen.findByRole("link", { name: "Install a plugin" });
    expect(install).toHaveAttribute("href", "/plugins");
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
    expect(
      await screen.findByText("Keeping it up to date", {}, { timeout: 15000 }),
    ).toBeInTheDocument();
    expect(screen.getByText("Downloaded files")).toBeInTheDocument();
    expect(screen.queryByText("New releases")).not.toBeInTheDocument();
    expect(screen.queryByText("Source: TMDB")).not.toBeInTheDocument();
  });

  it("sets the quality profile on a library from its settings", async () => {
    const { fake } = renderApp("/libraries/1/settings");
    await screen.findByText("Quality profile", {}, { timeout: 15000 });
    fireEvent.click(screen.getByRole("button", { name: "Quality profile" }));
    fireEvent.click(await screen.findByRole("option", { name: "4K" }, { timeout: 15000 }));
    await settleUi(() => {
      const patch = calls(fake, "PATCH", "/libraries/1").at(-1);
      expect(patch?.body).toMatchObject({ qualityProfile: "4K" });
    });
  });
});
