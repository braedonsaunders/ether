import { describe, expect, it } from "vitest";
import { normalizeRules } from "../pages/viewers/shared";
import {
  ORIGINAL_AUDIO,
  freshHouseholdLanguage,
  metadataFor,
  projectHouseholdRules,
  regionFromLocales,
  savedHouseholdLanguage,
  withConsoleLanguage,
} from "../pages/setup/language";

describe("setup language choice", () => {
  it("maps a Canadian French browser onto French metadata, Canada, and French tracks", () => {
    const choice = freshHouseholdLanguage(["fr-CA", "fr", "en-US"]);
    expect(choice.console).toBe("fr");
    expect(choice.region).toBe("CA");
    expect(choice.metadata).toBe("fr-CA");
    expect(choice.audio).toBe("fr");
    expect(choice.subtitle).toBe("fr");
  });

  it("keeps the browser region when the console language changes", () => {
    const choice = withConsoleLanguage(freshHouseholdLanguage(["en-CA"]), "ja");
    expect(choice.console).toBe("ja");
    expect(choice.region).toBe("CA");
    expect(choice.metadata).toBe("ja-JP");
    expect(choice.audio).toBe("ja");
    expect(choice.subtitle).toBe("ja");
  });

  it("uses a known catalogue region and falls back when the tag has none", () => {
    expect(regionFromLocales(["zh-Hans-CN", "en"])).toBe("CN");
    expect(regionFromLocales(["en"])).toBe("US");
    expect(metadataFor("pt-BR", "PT")).toBe("pt-PT");
    expect(metadataFor("en", "GB")).toBe("en-GB");
  });

  it("writes household defaults without changing subtitle mode", () => {
    const existing = normalizeRules({
      subtitleMode: "foreign-only",
      subtitleStyle: "sdh",
      audioLanguages: [],
      subtitleLanguages: [],
    });
    const choice = freshHouseholdLanguage(["ja-JP"]);
    const rules = projectHouseholdRules(existing, { ...choice, audio: "ja", subtitle: "en" });
    expect(rules.audioLanguages).toEqual(["ja"]);
    expect(rules.subtitleLanguages).toEqual(["en"]);
    expect(rules.subtitleMode).toBe("foreign-only");
    expect(rules.subtitleStyle).toBe("sdh");
    expect(rules.preferOriginal).toBe(false);
  });

  it("ranks original audio first only when that option is chosen", () => {
    const existing = normalizeRules({ audioLanguages: ["en"], subtitleLanguages: ["en"] });
    const original = projectHouseholdRules(existing, {
      ...freshHouseholdLanguage(["en-US"]),
      audio: ORIGINAL_AUDIO,
      subtitle: "en",
    });
    expect(original.preferOriginal).toBe(true);
    expect(original.audioLanguages).toEqual([]);
    expect(original.subtitleLanguages).toEqual(["en"]);
    const kept = projectHouseholdRules(
      { ...original, audioLanguages: ["ja"] },
      { ...freshHouseholdLanguage(["en-US"]), audio: ORIGINAL_AUDIO, subtitle: "en" },
    );
    expect(kept.audioLanguages).toEqual(["ja"]);
    expect(kept.preferOriginal).toBe(true);
  });

  it("shows a finished server's saved languages instead of the browser", () => {
    const choice = savedHouseholdLanguage({
      remembered: "en",
      catalogLanguage: "ja-JP",
      catalogRegion: "JP",
      audioLanguages: ["ja"],
      subtitleLanguages: ["en"],
      preferOriginal: false,
    });
    expect(choice).toMatchObject({
      console: "en",
      metadata: "ja-JP",
      region: "JP",
      audio: "ja",
      subtitle: "en",
    });
  });
});
