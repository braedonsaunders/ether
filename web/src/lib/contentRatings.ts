import { localized, t } from "../i18n";
import type { FacetValue } from "./contract";

/**
 * Content ratings are compared by age level on the server: "PG" covers PG,
 * TV-PG and other countries' equivalents (gb/PG, se/11…). A rule or viewer
 * limit stores the level's key, the US film label where there is one.
 */
export const NOT_RATED = "NR";

export interface RatingLevel {
  /** Stored key. */
  value: string;
  /** The US film and TV labels of the level, as one line. */
  labels: string;
  /** Plain-language name. */
  name: string;
}

export const RATING_LEVELS = localized<RatingLevel[]>(() => [
  { value: "G", labels: "G · TV-G · TV-Y", name: t("All ages") },
  { value: "PG", labels: "PG · TV-PG · TV-Y7", name: t("Parental guidance") },
  { value: "PG-13", labels: "PG-13 · TV-14", name: t("Teens 13+") },
  { value: "R", labels: "R · TV-MA", name: t("Mature 16+") },
  { value: "NC-17", labels: "NC-17", name: t("Adults 18+") },
]);

/** US labels that are not a level's key, mapped to that key. */
const US_ALIASES: Record<string, string> = {
  "TV-G": "G",
  "TV-Y": "G",
  "TV-Y7": "PG",
  "TV-Y7-FV": "PG",
  "TV-PG": "PG",
  "TV-14": "PG-13",
  "TV-MA": "R",
  "NOT RATED": NOT_RATED,
  UNRATED: NOT_RATED,
};

/**
 * The level key a stored rule or limit value stands for. US labels map
 * locally; other boards' labels map through a content-rating facet's
 * equivalents. A value no level lists is returned unchanged.
 */
export function canonicalRating(value: string, facet?: FacetValue[] | null): string {
  const v = value.trim();
  const upper = v.toUpperCase();
  if (RATING_LEVELS.some((l) => l.value === upper) || upper === NOT_RATED) return upper;
  if (US_ALIASES[upper]) return US_ALIASES[upper];
  const hit = facet?.find((f) => f.labels?.some((l) => l.toUpperCase() === upper));
  return hit?.key ?? v;
}

/** "PG · TV-PG" for a level key, "Not rated" for NR, the value otherwise. */
export function ratingLevelLabel(value: string): string {
  const key = canonicalRating(value);
  if (key === NOT_RATED) return t("Not rated");
  return RATING_LEVELS.find((l) => l.value === key)?.labels ?? value;
}

/** A stored certification for display: "ca/G" reads "G (CA)". */
export function formatRating(stored: string): string {
  const m = /^([a-z]{2}(?:-[a-z]{2})?)\/(.+)$/.exec(stored);
  return m?.[1] && m[2] ? `${m[2]} (${m[1].toUpperCase()})` : stored;
}

const EXAMPLE_BOARDS = ["gb", "ca", "au", "de", "fr", "br"];

/** A few other boards' labels for a level, e.g. "U (GB) · 0 (DE)". */
export function ratingExamples(labels: string[] | undefined, max = 3): string {
  const out: string[] = [];
  for (const board of EXAMPLE_BOARDS) {
    const hit = labels?.find((l) => l.startsWith(`${board}/`));
    if (hit && out.length < max) out.push(formatRating(hit));
  }
  return out.join(" · ");
}

const RELEASE_TYPES = localized<Record<string, string>>(() => ({
  "1": t("premiere"),
  "2": t("limited release"),
  "3": t("theatrical release"),
  "4": t("digital release"),
  "5": t("physical release"),
  "6": t("TV premiere"),
}));

/** Where a rating came from, e.g. "TMDB · US · TV premiere" or "OMDb (IMDb)". */
export function ratingSource(source: string): string {
  if (source === "omdb") return t("OMDb (IMDb)");
  if (source === "mdblist") return "MDBList";
  const [provider, country, release] = source.split(":");
  if (provider !== "tmdb" || !country) return source;
  const kind = release ? RELEASE_TYPES[release] : undefined;
  return kind
    ? t("TMDB · {country} · {release}", { country, release: kind })
    : t("TMDB · {country}", { country });
}
