import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { calls, renderApp } from "./harness";

const WAIT = { timeout: 4000 };

/** Saves only, not previews. */
const imports = (fake: Parameters<typeof calls>[0]) =>
  calls(fake, "POST", "/releases/quality-import").filter(
    (c) => c.path === "/releases/quality-import",
  );

async function openImport() {
  fireEvent.click(await screen.findByRole("button", { name: "Add profile" }, WAIT));
  fireEvent.click(
    await screen.findByRole("button", { name: "Import a preset quality profile" }, WAIT),
  );
  return screen.findByRole("dialog", { name: "Import a quality profile" }, WAIT);
}

describe("Quality profile import from guides", () => {
  it("previews a TRaSH profile and adds it with its custom format scores", async () => {
    const { fake } = renderApp("/quality/profiles");
    const drawer = await openImport();
    const add = within(drawer).getByRole("button", { name: "Add profile" });
    expect(add).toBeDisabled();

    fireEvent.click(await within(drawer).findByRole("button", { name: "TRaSH profile" }, WAIT));
    fireEvent.click(await screen.findByRole("option", { name: /\[SQP\] SQP-1 \(2160p\)/ }, WAIT));

    const table = await within(drawer).findByRole("table", { name: "Custom formats" }, WAIT);
    const rows = within(table).getAllByRole("row");
    // Sorted by score; the set's score wins over the default.
    expect(rows[1]).toHaveTextContent("WEB Tier 01");
    expect(rows[1]).toHaveTextContent("+1,800");
    expect(rows[1]).toHaveTextContent("Added");
    expect(
      within(drawer).getByText(
        "This Special Quality Profile is an advanced Quality Profile designed for a specific use case.",
      ),
    ).toBeInTheDocument();
    expect(within(drawer).getByText(/Scored with TRaSH's sqp-1-2160p scores/)).toBeInTheDocument();
    expect(within(drawer).getByText("Blu-ray 4K")).toBeInTheDocument();
    expect(within(drawer).getByText("Bluray-2160p › WEB 2160p")).toBeInTheDocument();
    expect(within(drawer).getByText(/Left out SDTV/)).toBeInTheDocument();
    expect(within(drawer).getByText("1,000")).toBeInTheDocument();
    // The drawer names the source profile and the media it applies to.
    expect(within(drawer).getByText("Source profile: sqp-1-2160p")).toBeInTheDocument();
    expect(within(drawer).getByText("Movies")).toBeInTheDocument();

    // "4K" is taken by an existing profile in the fake; rename before adding.
    fireEvent.change(within(drawer).getByLabelText("Name"), { target: { value: "4K" } });
    fireEvent.click(add);
    expect(
      await within(drawer).findByText(/A profile named "4K" already exists/, {}, WAIT),
    ).toBeInTheDocument();

    fireEvent.change(within(drawer).getByLabelText("Name"), { target: { value: "UHD (TRaSH)" } });
    fireEvent.click(add);
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument(), WAIT);
    expect(imports(fake).at(-1)!.body).toEqual({
      app: "radarr",
      profile: "sqp-1-2160p",
      name: "UHD (TRaSH)",
    });
    expect(await screen.findByText("UHD (TRaSH)", {}, WAIT)).toBeInTheDocument();
    const saved = fake.state.config.quality.profiles!.find((p) => p.name === "UHD (TRaSH)")!;
    expect(saved.customFormatScores).toMatchObject({ c20f169ef63c5f40c2def54abaf4438e: 1800 });
    expect(saved.minCustomFormatScore).toBe(1000);

    // The profile's drawer shows what the import carried over.
    fireEvent.click(screen.getByRole("button", { name: "Edit UHD (TRaSH)" }));
    const editor = await screen.findByRole("dialog", { name: "UHD (TRaSH)" }, WAIT);
    expect(within(editor).getByText("Allowed qualities")).toBeInTheDocument();
    expect(within(editor).getAllByText("WEB-DL 4K").length).toBeGreaterThan(0);
    expect(within(editor).getByLabelText("Minimum score")).toHaveValue(1000);
    // The imported profile's media and source are read-only.
    expect(
      within(editor).getByText("Set by the Radarr profile it was imported from."),
    ).toBeInTheDocument();
    expect(within(editor).getByText("TRaSH Guides")).toBeInTheDocument();
    expect(within(editor).queryByRole("radio", { name: "Both" })).not.toBeInTheDocument();
  });

  it("shows own and default format counts in the picker", async () => {
    renderApp("/quality/profiles");
    const drawer = await openImport();
    fireEvent.click(await within(drawer).findByRole("button", { name: "TRaSH profile" }, WAIT));
    const option = await screen.findByRole("option", { name: /\[SQP\] SQP-1/ }, WAIT);
    expect(option).toHaveTextContent("3 own · 32 default");
  });

  it("warns and permits a profile with an unreachable preferred score", async () => {
    const { fake } = renderApp("/quality/profiles");
    const drawer = await openImport();
    fireEvent.click(await within(drawer).findByRole("button", { name: "TRaSH profile" }, WAIT));
    fireEvent.click(await screen.findByRole("option", { name: /Needs a perfect release/ }, WAIT));
    expect(await within(drawer).findByText(/prefers a score of 100/, {}, WAIT)).toBeInTheDocument();
    expect(within(drawer).getByRole("button", { name: "Add profile" })).toBeEnabled();
    expect(imports(fake)).toHaveLength(0);
  });

  it("previews and imports a Profilarr (Dictionarry) quality profile", async () => {
    const { fake } = renderApp("/quality/profiles");
    const drawer = await openImport();
    fireEvent.click(within(drawer).getByRole("radio", { name: "Profilarr (Dictionarry)" }));

    fireEvent.click(await within(drawer).findByRole("button", { name: "Profilarr profile" }, WAIT));
    fireEvent.click(await screen.findByRole("option", { name: /2160p Remux/ }, WAIT));

    const table = await within(drawer).findByRole("table", { name: "Custom formats" }, WAIT);
    const rows = within(table).getAllByRole("row");
    expect(rows[1]).toHaveTextContent("2160p Remux");
    expect(rows[1]).toHaveTextContent("+860,000");
    expect(within(drawer).getByText("Remux 4K")).toBeInTheDocument();
    expect(within(drawer).getByText("200,000")).toBeInTheDocument();

    fireEvent.click(within(drawer).getByRole("button", { name: "Add profile" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument(), WAIT);
    expect(imports(fake).at(-1)!.body).toEqual({
      app: "radarr",
      source: "dictionarry",
      profile: "2160p-remux",
      name: "2160p Remux",
    });
    expect(await screen.findByText("2160p Remux", {}, WAIT)).toBeInTheDocument();
  });

  it("previews pasted profile JSON for the app its qualities name", async () => {
    const { fake } = renderApp("/quality/profiles");
    const drawer = await openImport();
    fireEvent.click(within(drawer).getByRole("radio", { name: "Paste JSON" }));
    const preview = within(drawer).getByRole("button", { name: "Preview" });
    expect(preview).toBeDisabled();

    fireEvent.change(within(drawer).getByLabelText("Profile JSON"), { target: { value: "{" } });
    fireEvent.click(preview);
    expect(
      await within(drawer).findByText("Not valid profile JSON.", {}, WAIT),
    ).toBeInTheDocument();

    const json = JSON.stringify({
      name: "My remuxes",
      items: [{ name: "Bluray-1080p Remux", allowed: true }],
      formatItems: { "BR-DISK": "85c61753df5da1fb2aab6f2a47426b09", Mystery: "nope" },
    });
    fireEvent.change(within(drawer).getByLabelText("Profile JSON"), { target: { value: json } });
    fireEvent.click(within(drawer).getByRole("button", { name: "Preview" }));
    expect(await within(drawer).findByText(/This profile is for Shows/)).toBeInTheDocument();
    expect(within(drawer).getByRole("button", { name: "Add profile" })).toBeDisabled();
    fireEvent.click(within(drawer).getByRole("button", { name: "Close" }));
    fireEvent.click(screen.getByRole("radio", { name: "Shows" }));
    const showsDrawer = await openImport();
    expect(within(showsDrawer).getByText("Shows (Sonarr)")).toBeInTheDocument();
    fireEvent.click(within(showsDrawer).getByRole("radio", { name: "Paste JSON" }));
    fireEvent.change(within(showsDrawer).getByLabelText("Profile JSON"), {
      target: { value: json },
    });
    fireEvent.click(within(showsDrawer).getByRole("button", { name: "Preview" }));
    const table = await within(showsDrawer).findByRole("table", { name: "Custom formats" }, WAIT);
    expect(within(table).getByText("Not found")).toBeInTheDocument();
    expect(within(showsDrawer).getByText("Remux 1080p")).toBeInTheDocument();
    fireEvent.click(within(showsDrawer).getByRole("button", { name: "Add profile" }));
    await waitFor(() => expect(imports(fake)).toHaveLength(1));
    expect(imports(fake)[0]!.body).toEqual({
      app: "sonarr",
      json,
      name: "My remuxes",
    });
    // A show profile is listed under Shows, not Movies.
    expect(await screen.findByText("My remuxes", {}, WAIT)).toBeInTheDocument();
  });
});
