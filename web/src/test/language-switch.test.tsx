import { systemMessageKey, type MessageCatalog } from "@braedonsaunders/appkit-i18n";
import { Toaster } from "@braedonsaunders/appkit-ui";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { getLocale, loadLocale, type CatalogLoader } from "../i18n";
import de from "../i18n/locales/de.json";
import { I18nProvider, LanguagePicker } from "../i18n/react";
import { calls, renderApp } from "./harness";

const german = (source: string) => {
  const entry = (de as MessageCatalog)[systemMessageKey(source)];
  if (typeof entry !== "string") throw new Error(`no German copy for ${source}`);
  return entry;
};

async function pick(language: RegExp, pickerName = "Display language") {
  fireEvent.click(screen.getAllByRole("button", { name: pickerName })[0]!);
  fireEvent.click(await screen.findByRole("menuitemradio", { name: language }));
}

afterEach(async () => {
  localStorage.clear();
  await loadLocale("en");
});

describe("switching language", () => {
  it("keeps unsaved edits and re-renders the page in the new language", async () => {
    const { fake } = renderApp("/catalog/settings");
    const popularity = await screen.findByLabelText("Minimum popularity");
    fireEvent.change(popularity, { target: { value: "42" } });
    const gate = screen.getByLabelText("Wait for digital or physical release");
    fireEvent.click(gate);
    expect(screen.getByRole("button", { name: "Save changes" })).toBeInTheDocument();

    await pick(/Deutsch/);

    // The same form, still dirty, now labelled in German.
    const saveDe = await screen.findByRole("button", { name: german("Save changes") });
    expect(screen.getByLabelText(german("Minimum popularity"))).toHaveValue(42);
    expect(screen.getByLabelText(german("Wait for digital or physical release"))).toBeChecked();
    expect(screen.queryByRole("button", { name: "Save changes" })).toBeNull();
    expect(getLocale()).toBe("de");
    expect(localStorage.getItem("ether.locale")).toBe("de");

    fireEvent.click(saveDe);
    await waitFor(() => expect(calls(fake, "PUT", "/config/catalog").length).toBe(1));
    expect(calls(fake, "PUT", "/config/catalog")[0]?.body).toMatchObject({
      minPopularity: 42,
      gateMoviesByHomeRelease: true,
    });
  });
});

describe("language picker loading", () => {
  const harness = (load: CatalogLoader) =>
    render(
      <I18nProvider load={load}>
        {() => (
          <>
            <LanguagePicker side="bottom" />
            <Toaster />
          </>
        )}
      </I18nProvider>,
    );

  it("keeps the current language and reports a catalogue that fails to load", async () => {
    harness(() => Promise.reject(new Error("chunk failed to load")));
    await pick(/Deutsch/);
    expect(
      await screen.findByText("Couldn't load Deutsch. Check your connection and try again."),
    ).toBeInTheDocument();
    expect(getLocale()).toBe("en");
    expect(localStorage.getItem("ether.locale")).toBeNull();
    expect(
      within(screen.getByRole("button", { name: "Display language" })).getByText("English"),
    ).toBeInTheDocument();
  });

  it("settles on the last language chosen when an earlier load finishes later", async () => {
    let finishGerman!: (catalog: MessageCatalog) => void;
    harness((locale) =>
      locale === "de" ? new Promise((resolve) => (finishGerman = resolve)) : Promise.resolve({}),
    );
    await pick(/Deutsch/);
    await pick(/Français/);
    await waitFor(() => expect(getLocale()).toBe("fr"));
    finishGerman({});
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(getLocale()).toBe("fr");
    expect(localStorage.getItem("ether.locale")).toBe("fr");
    expect(screen.getByRole("button", { name: "Display language" })).toHaveTextContent("Français");
  });
});
