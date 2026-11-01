import { describe, expect, it } from "vitest";
import {
  canonicalRating,
  formatRating,
  ratingExamples,
  ratingLevelLabel,
  ratingSource,
} from "./contentRatings";
import { describeRule } from "./rules";

describe("content ratings", () => {
  it("maps US film and TV labels to one level key", () => {
    expect(["G", "TV-G", "tv-y"].map((v) => canonicalRating(v))).toEqual(["G", "G", "G"]);
    expect(canonicalRating("TV-PG")).toBe("PG");
    expect(canonicalRating("TV-14")).toBe("PG-13");
    expect(canonicalRating("TV-MA")).toBe("R");
    expect(canonicalRating("Not Rated")).toBe("NR");
  });
  it("maps other boards' labels through the facet's equivalents", () => {
    const facet = [{ key: "PG", title: "PG · TV-PG", count: 3, labels: ["PG", "TV-PG", "gb/PG"] }];
    expect(canonicalRating("gb/PG", facet)).toBe("PG");
    expect(canonicalRating("L")).toBe("L");
  });
  it("reads stored ratings and their sources for people", () => {
    expect(formatRating("ca/G")).toBe("G (CA)");
    expect(formatRating("ca-qc/13+")).toBe("13+ (CA-QC)");
    expect(formatRating("U/A 13+")).toBe("U/A 13+");
    expect(ratingLevelLabel("TV-PG")).toBe("PG · TV-PG · TV-Y7");
    expect(canonicalRating("TV-Y7")).toBe("PG");
    expect(ratingLevelLabel("NR")).toBe("Not rated");
    expect(ratingExamples(["G", "TV-G", "de/0", "gb/U", "br/L", "ca/G"])).toBe(
      "U (GB) · G (CA) · 0 (DE)",
    );
    expect(ratingSource("tmdb:US:6")).toBe("TMDB · US · TV premiere");
    expect(ratingSource("tmdb:CA")).toBe("TMDB · CA");
    expect(ratingSource("omdb")).toBe("OMDb (IMDb)");
  });
  it("describes rating rules by level", () => {
    expect(describeRule({ field: "contentRating", op: "notIn", value: ["G", "TV-PG", "NR"] })).toBe(
      "Content rating is none of G · TV-G · TV-Y, PG · TV-PG · TV-Y7, Not rated",
    );
    expect(describeRule({ field: "contentRating", op: "lte", value: "TV-14" })).toBe(
      "Content rating is at most PG-13 · TV-14",
    );
  });
});
