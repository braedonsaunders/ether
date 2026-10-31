import { t } from "../../i18n";

/**
 * Household app ids and one-line purposes shared with the first-run chooser.
 * Brand marks live in `integration-brands`; this file does not draw logos.
 */
export const SETUP_APP_IDS = ["plex", "jellyfin", "emby", "silo", "stremio", "webdav"] as const;

export function appPath(id: string): string {
  return id === "webdav" ? "files" : id;
}

export function appLabel(id: string, fallbackName: string): string {
  if (id === "webdav") return t("Files");
  if (id === "m3u") return t("M3U and XMLTV");
  return fallbackName;
}

/** One human sentence for an overview card or the setup chooser. */
export function appPurpose(id: string): string {
  switch (id) {
    case "plex":
      return t("At home and away, through plex.tv.");
    case "jellyfin":
      return t("At home. Away from home, use a VPN such as Tailscale or an address you publish.");
    case "emby":
      return t("At home. Away from home, use a VPN such as Tailscale or an address you publish.");
    case "silo":
      return t("At home on your network.");
    case "stremio":
      return t("This library as a Stremio addon.");
    case "webdav":
      return t("This library as files in other apps.");
    case "dlna":
      return t("TVs and players that browse over DLNA.");
    case "m3u":
      return t("Live TV playlists for other apps.");
    default:
      return t("Another way to watch this library.");
  }
}
