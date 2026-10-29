import { localized, t } from "../../i18n";

export type Overlays = {
  enabled: boolean;
  status: boolean;
  quality: boolean;
  ratings: boolean;
  details: boolean;
  personalize: boolean;
  localize: boolean;
  style: "glass" | "solid" | "minimal";
  position: "bottom" | "top";
  accent: string;
  size: "small" | "medium" | "large";
};
export const normalizeOverlays = (s: Partial<Overlays>): Overlays => ({
  enabled: false,
  status: false,
  quality: false,
  ratings: false,
  details: false,
  personalize: false,
  localize: false,
  ...s,
  style: s.style || "glass",
  position: s.position || "bottom",
  accent: s.accent || "#d4a574",
  size: s.size || "medium",
});
export const PRESETS: {
  key: string;
  name: string;
  description: string;
  settings: Partial<Overlays>;
}[] = localized(() => [
  {
    key: "cinema",
    name: t("Cinema"),
    description: t("Glass · scores & details"),
    settings: {
      enabled: true,
      style: "glass",
      position: "bottom",
      accent: "#d4a574",
      size: "medium",
      details: true,
      quality: true,
      ratings: true,
      status: false,
    },
  },
  {
    key: "spotlight",
    name: t("Spotlight"),
    description: t("Accent · all available facts"),
    settings: {
      enabled: true,
      style: "solid",
      position: "top",
      accent: "#a7c4b5",
      size: "large",
      details: true,
      quality: true,
      ratings: true,
      status: true,
    },
  },
  {
    key: "essential",
    name: t("Essential"),
    description: t("Minimal · ratings only"),
    settings: {
      enabled: true,
      style: "minimal",
      position: "bottom",
      accent: "#e8e4dc",
      size: "small",
      quality: false,
      details: false,
      ratings: true,
      status: false,
    },
  },
]);
export function previewURL(
  settings: Overlays,
  key: string,
  user?: number | null,
  lang = "en",
  nonce = 0,
) {
  const params = new URLSearchParams({
    settings: JSON.stringify(settings),
    lang,
    v: String(nonce),
  });
  if (user != null) params.set("userId", String(user));
  return `/ether/api/discovery/overlays/preview/${encodeURIComponent(key)}?${params}`;
}

export type ArtworkExplanation = {
  labels: string[];
  layers: {
    key: "status" | "quality" | "ratings" | "details";
    state: "ready" | "unavailable" | "off";
    labels: string[];
    reason: string;
  }[];
};
export function explanationURL(
  settings: Overlays,
  key: string,
  user: number | null,
  lang: string,
  nonce: number,
) {
  return previewURL(settings, key, user, lang, nonce)
    .replace("/ether/api", "")
    .replace("/preview/", "/explain/");
}
