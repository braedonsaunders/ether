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
  fireEvent.click(await screen.findByRole("button", { name: "Import from TRaSH Guides" }, WAIT));
  return screen.findByRole("dialog", { name: "Import from TRaSH Guides" }, WAIT);
}

describe("Quality profile import from TRaSH Guides", () => {
  it("previews a TRaSH profile and adds it with its custom format scores", async () => {
    const { fake } = renderApp("/releases/profiles");
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
    expect(within(drawer).getByText("Blu-ray › WEB-DL › WEBRip")).toBeInTheDocument();
    expect(within(drawer).getByText(/Left out SDTV/)).toBeInTheDocument();
    expect(within(drawer).getByText("1,000")).toBeInTheDocument();

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
    expect(within(editor).getByText("Imported from TRaSH / Radarr / Sonarr")).toBeInTheDocument();
    expect(within(editor).getByText("WEB-DL 4K")).toBeInTheDocument();
    expect(within(editor).getByLabelText("Minimum format score")).toHaveValue(1000);
  });

  it("previews and imports a Profilarr (Dictionarry) quality profile", async () => {
    const { fake } = renderApp("/releases/profiles");
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
    const { fake } = renderApp("/releases/profiles");
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
    const table = await within(drawer).findByRole("table", { name: "Custom formats" }, WAIT);
    expect(within(table).getByText("Not found")).toBeInTheDocument();
    expect(within(drawer).getByText("Remux 1080p")).toBeInTheDocument();
    // The Sonarr remux name switched the app.
    expect(within(drawer).getByRole("radio", { name: "Sonarr (shows)" })).toHaveAttribute(
      "aria-checked",
      "true",
    );
    fireEvent.click(within(drawer).getByRole("button", { name: "Add profile" }));
    await waitFor(() => expect(imports(fake)).toHaveLength(1));
    expect(imports(fake)[0]!.body).toEqual({
      app: "sonarr",
      json,
      name: "My remuxes",
    });
    expect(await screen.findByText("My remuxes", {}, WAIT)).toBeInTheDocument();
  });
});
