/**
 * Every owner destination in one place. The sidebar and ⌘K search read this,
 * so a section only ever has one home.
 */

import { localized, t, msg, searchTerms } from "../i18n";

export interface Destination {
  href: string;
  title: string;
  /** Page the destination belongs to, shown as the search subtitle. */
  page: string;
  keywords?: string;
  /** A whole page rather than a section within one. */
  isPage?: boolean;
}

export interface PageDef {
  href: string;
  title: string;
  /** In-page sections (tabs) in display order. */
  sections: { key: string; title: string; keywords?: string }[];
  keywords?: string;
}

export const PAGES = localized<PageDef[]>(() => [
  {
    href: "/cache",
    title: t("Cache"),
    sections: [
      {
        key: "overview",
        title: t("Overview"),
        keywords: searchTerms(msg("storage readiness policy")),
      },
      {
        key: "explorer",
        title: t("Explorer"),
        keywords: searchTerms(msg("retained media releases pins")),
      },
      {
        key: "preloading",
        title: t("Preloading"),
        keywords: searchTerms(msg("household selection popular movies tv")),
      },
    ],
    keywords: searchTerms(msg("disk budget storage retention prefetch warming next episode")),
  },
  {
    href: "/",
    title: t("Overview"),
    sections: [],
    keywords: searchTerms(msg("home dashboard health attention status")),
  },
  {
    href: "/activity",
    title: t("Activity"),
    sections: [
      {
        key: "now",
        title: t("Now playing"),
        keywords: searchTerms(msg("streams sessions stop live")),
      },
      { key: "history", title: t("History"), keywords: searchTerms(msg("plays watched")) },
      {
        key: "stats",
        title: t("Stats"),
        keywords: searchTerms(msg("statistics devices hours viewers titles")),
      },
    ],
  },
  {
    href: "/viewers",
    title: t("Viewers"),
    sections: [
      { key: "people", title: t("People"), keywords: searchTerms(msg("users friends accounts")) },
      {
        key: "defaults",
        title: t("Defaults"),
        keywords: searchTerms(msg("audio subtitles language watchlist alerts policy")),
      },
    ],
  },
  {
    href: "/home",
    title: t("Home"),
    sections: [],
    keywords: searchTerms(msg("designer rows layout overrides plex home")),
  },
  {
    href: "/collections",
    title: t("Collections"),
    sections: [],
    keywords: searchTerms(msg("studio templates rows")),
  },
  {
    href: "/livetv",
    title: t("Live TV"),
    sections: [
      { key: "channels", title: t("Channels"), keywords: searchTerms(msg("virtual channel")) },
      {
        key: "sources",
        title: t("IPTV sources"),
        keywords: searchTerms(msg("m3u xmltv playlist")),
      },
      { key: "guide", title: t("Guide"), keywords: searchTerms(msg("epg schedule")) },
    ],
  },
  {
    href: "/catalog",
    title: t("Catalog"),
    sections: [
      {
        key: "settings",
        title: t("Catalog settings"),
        keywords: searchTerms(msg("tmdb language region popularity")),
      },
    ],
    keywords: searchTerms(msg("catalogue movies shows browse search library")),
  },
  {
    href: "/metadata",
    title: t("Metadata"),
    sections: [
      {
        key: "ratings",
        title: t("Ratings"),
        keywords: searchTerms(msg("omdb mdblist imdb rotten badges")),
      },
      {
        key: "artwork",
        title: t("Artwork"),
        keywords: searchTerms(msg("posters overlays backdrop")),
      },
      {
        key: "episodes",
        title: t("Episode order"),
        keywords: searchTerms(msg("dvd absolute aired ordering")),
      },
    ],
  },
  {
    href: "/sources",
    title: t("Sources"),
    sections: [
      {
        key: "usenet",
        title: t("Usenet providers"),
        keywords: searchTerms(msg("nntp backfill connections")),
      },
      {
        key: "indexers",
        title: t("Indexers"),
        keywords: searchTerms(
          msg(
            "newznab torznab torrent built-in sites definitions cardigann flaresolverr prowlarr stremio addon budget api",
          ),
        ),
      },
      {
        key: "debrid",
        title: t("Debrid"),
        keywords: searchTerms(
          msg(
            "real-debrid realdebrid torbox alldebrid premiumize debrid-link debridlink offcloud torrent cached",
          ),
        ),
      },
      {
        key: "availability",
        title: t("Availability"),
        keywords: searchTerms(msg("release feeds rss source evidence checks budget")),
      },
      {
        key: "usage",
        title: t("Usage & costs"),
        keywords: searchTerms(msg("quota price plan cost")),
      },
    ],
  },
  {
    href: "/releases",
    title: t("Releases"),
    sections: [
      {
        key: "quality",
        title: t("Quality profiles"),
        keywords: searchTerms(msg("resolution 4k 1080p remote")),
      },
      { key: "rules", title: t("Client rules"), keywords: searchTerms(msg("device profile")) },
      {
        key: "selection",
        title: t("Selection rules"),
        keywords: searchTerms(msg("cam health blocked terms groups")),
      },
      {
        key: "scoring",
        title: t("Custom scoring"),
        keywords: searchTerms(msg("trash custom formats")),
      },
      {
        key: "startup",
        title: t("Playback start"),
        keywords: searchTerms(msg("startup race warm")),
      },
      {
        key: "pinned",
        title: t("Pinned"),
        keywords: searchTerms(msg("pin release override search interactive manual pick find")),
      },
      {
        key: "blocklist",
        title: t("Blocklist"),
        keywords: searchTerms(msg("blocked bad release unblock")),
      },
      {
        key: "history",
        title: t("History"),
        keywords: searchTerms(msg("traces resolver why release debug decisions")),
      },
    ],
  },
  {
    href: "/playback",
    title: t("Playback"),
    sections: [
      {
        key: "transcoding",
        title: t("Transcoding"),
        keywords: searchTerms(msg("ffmpeg hardware gpu bitrate")),
      },
      {
        key: "devices",
        title: t("Device capabilities"),
        keywords: searchTerms(msg("compatibility codecs")),
      },
      {
        key: "markers",
        title: t("Markers"),
        keywords: searchTerms(msg("intro credits skip chapters")),
      },
      {
        key: "subtitles",
        title: t("Subtitles"),
        keywords: searchTerms(msg("timing sync opensubtitles")),
      },
    ],
  },
  {
    href: "/notifications",
    title: t("Notifications"),
    sections: [
      {
        key: "channels",
        title: t("Channels"),
        keywords: searchTerms(msg("email discord webhook push alerts")),
      },
      { key: "report", title: t("Weekly report"), keywords: searchTerms(msg("newsletter digest")) },
      { key: "deliveries", title: t("Delivery history") },
    ],
  },
  {
    href: "/server",
    title: t("General"),
    sections: [
      {
        key: "server",
        title: t("Server"),
        keywords: searchTerms(msg("name port listen log level stream limit")),
      },
      {
        key: "downloads",
        title: t("Downloads"),
        keywords: searchTerms(msg("offline sync friends remux")),
      },
      {
        key: "personalize",
        title: t("Personalization"),
        keywords: searchTerms(msg("post-play up next match finish time clock")),
      },
      {
        key: "extras",
        title: t("Trailers & extras"),
        keywords: searchTerms(msg("cinema trailers yt-dlp resolver")),
      },
    ],
    keywords: searchTerms(msg("settings")),
  },
  {
    href: "/apps",
    title: t("Apps"),
    sections: [
      {
        key: "remote",
        title: t("Remote access"),
        keywords: searchTerms(msg("upnp port forward public")),
      },
      {
        key: "secure",
        title: t("Secure connections"),
        keywords: searchTerms(msg("certificate plex.direct tls")),
      },
    ],
    keywords: searchTerms(msg("settings plex apps protocols")),
  },
  {
    href: "/services",
    title: t("Integrations"),
    sections: [
      {
        key: "overview",
        title: t("Integrations"),
        keywords: searchTerms(msg("status tmdb debrid prowlarr ratings")),
      },
      { key: "trakt", title: "Trakt", keywords: searchTerms(msg("sync history")) },
      { key: "simkl", title: "Simkl", keywords: searchTerms(msg("simkl sync history")) },
      {
        key: "llm",
        title: t("Language model"),
        keywords: searchTerms(msg("llm ollama openai recaps search")),
      },
      { key: "opensubtitles", title: "OpenSubtitles", keywords: searchTerms(msg("subtitles")) },
    ],
    keywords: searchTerms(msg("settings integrations")),
  },
  {
    href: "/cluster",
    title: t("Cluster"),
    sections: [],
    keywords: searchTerms(
      msg("high availability ha nodes failover postgres postgresql join token weight leader"),
    ),
  },
  {
    href: "/maintenance",
    title: t("Maintenance"),
    sections: [],
    keywords: searchTerms(msg("server maintenance")),
  },
  {
    href: "/maintenance/backups",
    title: t("Backups & import"),
    sections: [
      {
        key: "backups",
        title: t("Backups"),
        keywords: searchTerms(msg("snapshot restore schedule")),
      },
      {
        key: "import",
        title: t("Import & export"),
        keywords: searchTerms(msg("migrate plex csv letterboxd trakt")),
      },
    ],
  },
  {
    href: "/maintenance/updates",
    title: t("Updates"),
    sections: [],
    keywords: searchTerms(msg("version channel beta maintenance window update now install")),
  },
  {
    href: "/maintenance/troubleshooting",
    title: t("Troubleshooting"),
    sections: [],
    keywords: searchTerms(msg("local file test playback")),
  },
  {
    href: "/maintenance/logs",
    title: t("Logs"),
    sections: [],
    keywords: searchTerms(msg("errors debug")),
  },
]);

export function destinations(): Destination[] {
  return PAGES.flatMap((p) => [
    { href: p.href, title: p.title, page: t("Page"), keywords: p.keywords, isPage: true },
    ...p.sections.map((s) => ({
      href: `${p.href}/${s.key}`,
      title: s.title,
      page: p.title,
      keywords: s.keywords,
    })),
  ]);
}
