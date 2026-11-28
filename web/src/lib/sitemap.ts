/**
 * Every owner destination in one place, derived from the descriptor
 * registry's page descriptors (internal/app/extend_console.go). The sidebar
 * and ⌘K search read this, so a section only ever has one home.
 */

import { localized, msg, searchTerms, t } from "../i18n";
import { settingHref } from "../components/SettingFocus";
import { textOf, type Descriptor } from "./extend";
import { pageRoute, searchTextOf, type PageMeta } from "./console-descriptors";

interface Destination {
  href: string;
  title: string;
  /** Page the destination belongs to, shown as the search subtitle. */
  page: string;
  keywords?: string;
  /** A whole page rather than a section within one. */
  isPage?: boolean;
}

interface PageDef {
  href: string;
  title: string;
  /** In-page sections (tabs) in display order. */
  sections: { key: string; title: string; keywords?: string; href?: string }[];
  keywords?: string;
}

const PAGES = localized<PageDef[]>(() => [
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
        keywords: searchTerms(msg("household selection popular movies tv warm arrivals")),
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
      {
        key: "watchlists",
        title: t("Watchlists"),
        keywords: searchTerms(msg("Watchlist monitoring")),
      },
      {
        key: "security",
        title: t("Security"),
        keywords: searchTerms(msg("Sign-in lockouts")),
      },
    ],
    keywords: searchTerms(msg("users friends accounts people")),
  },
  {
    href: "/recommendations",
    title: t("Recommendations"),
    sections: [],
    keywords: searchTerms(
      msg("home rows pages designer layout personalized curated library recommended plex"),
    ),
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
      { key: "guide", title: t("Guide"), keywords: searchTerms(msg("epg schedule")) },
      {
        key: "looks",
        title: t("Styles"),
        keywords: searchTerms(msg("on air graphics brand reels")),
      },
    ],
  },
  {
    href: "/calendar",
    title: t("Calendar"),
    sections: [
      { key: "month", title: t("Month"), keywords: searchTerms(msg("schedule grid")) },
      { key: "week", title: t("Week") },
      { key: "agenda", title: t("Agenda"), keywords: searchTerms(msg("list upcoming")) },
    ],
    keywords: searchTerms(
      msg("upcoming episodes premieres finales releases airing tonight sonarr radarr schedule"),
    ),
  },
  {
    href: "/libraries",
    title: t("Libraries"),
    sections: [
      {
        key: "1/rules",
        title: t("Movies · Rules"),
        keywords: searchTerms(msg("hide show rules movie exclusions visibility")),
      },
      {
        key: "1/settings",
        title: t("Movies · Settings"),
        keywords: searchTerms(msg("tmdb language region obscure new releases playable digital")),
      },
      {
        key: "2/rules",
        title: t("TV Shows · Rules"),
        keywords: searchTerms(msg("hide show rules tv exclusions visibility")),
      },
      {
        key: "2/settings",
        title: t("TV Shows · Settings"),
        keywords: searchTerms(msg("tmdb language region obscure new releases episodes")),
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
        key: "debrid",
        title: t("Debrid"),
        keywords: searchTerms(
          msg(
            "real-debrid realdebrid torbox alldebrid premiumize debrid-link debridlink offcloud torrent cached",
          ),
        ),
      },
      {
        key: "usenet",
        title: t("Usenet"),
        keywords: searchTerms(msg("nntp backfill connections")),
      },
      {
        key: "iptv",
        title: t("IPTV"),
        keywords: searchTerms(msg("m3u xmltv playlist")),
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
        key: "statistics",
        title: t("Statistics"),
        keywords: searchTerms(
          msg("stats dashboard success speed plays failover cache data downloaded debrid"),
        ),
      },
      {
        key: "usage",
        title: t("Usage"),
        keywords: searchTerms(msg("usage quota price plan cost per play iptv")),
      },
    ],
  },
  {
    href: "/releases",
    title: t("Releases"),
    keywords: searchTerms(msg("quality")),
    sections: [
      {
        key: "profiles",
        title: t("Profiles"),
        keywords: searchTerms(
          msg(
            "quality profiles resolution 4k 1080p remote away bitrate cam scoring trash custom formats ranking source order usenet debrid versions",
          ),
        ),
      },
      {
        key: "rules",
        title: t("Rules"),
        keywords: searchTerms(msg("device client viewer profile max quality limit")),
      },
      {
        key: "check",
        title: t("Who gets what"),
        keywords: searchTerms(msg("who gets what viewer app device resolution hdr bitrate")),
      },
      {
        key: "searching",
        title: t("Searching"),
        keywords: searchTerms(
          msg("how ether searches start speed cache health samples candidates"),
        ),
      },
      {
        key: "blocklist",
        title: t("Blocklist"),
        keywords: searchTerms(msg("blocked bad release unblock terms groups")),
      },
      {
        key: "pinned",
        title: t("Pinned"),
        keywords: searchTerms(msg("pin release override search interactive manual pick find")),
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
        key: "audio",
        title: t("Audio"),
        keywords: searchTerms(
          msg("audio languages original dub loudness night mode dialogue household"),
        ),
      },
      {
        key: "subtitles",
        title: t("Subtitles"),
        keywords: searchTerms(
          msg("subtitles languages appearance style timing sync drift fix corrected copy"),
        ),
      },
      {
        key: "transcoding",
        title: t("Streams & transcoding"),
        keywords: searchTerms(msg("ffmpeg hardware gpu streams at once limit rtx")),
      },
      {
        key: "devices",
        title: t("App compatibility"),
        keywords: searchTerms(msg("compatibility codecs")),
      },
      {
        key: "extras",
        title: t("Extras"),
        keywords: searchTerms(msg("intro credits skip chapters up next post-play")),
      },
      {
        key: "prerolls",
        title: t("Pre-rolls"),
        keywords: searchTerms(
          msg("pre-roll preroll intro video before feature trailers cinema yt-dlp"),
        ),
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
    sections: [],
    keywords: searchTerms(
      msg("settings server name app ports local remote URLs listen proxy vpn setup wizard"),
    ),
  },
  {
    href: "/apps",
    title: t("Apps"),
    sections: [
      {
        key: "overview",
        title: t("Away from home"),
        keywords: searchTerms(msg("public address host port custom urls certificate https")),
      },
      {
        key: "account",
        href: "/apps/plex/account",
        title: t("Plex account"),
        keywords: searchTerms(msg("plex sign in claim account compatibility version")),
      },
      {
        key: "remote",
        href: "/apps/plex/remote",
        title: t("Remote access"),
        keywords: searchTerms(msg("upnp port forward secure connections plex.direct tls")),
      },
      {
        key: "downloads",
        href: "/apps/plex/downloads",
        title: t("Downloads"),
        keywords: searchTerms(msg("offline sync friends remux")),
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
      { key: "postersplus", href: "/services/overview#postersplus", title: "PostersPlus" },
      { key: "xrdb", href: "/services/overview#xrdb", title: "XRDB" },
      { key: "betterposters", href: "/services/overview#betterposters", title: "BetterPosters" },
      { key: "rpdb", href: "/services/overview#rpdb", title: "RPDB" },
      { key: "custom", href: "/services/overview#custom", title: t("Custom posters") },
      { key: "posterflow", href: "/services/overview#posterflow", title: "PosterFlow" },
      { key: "tmdb", title: "TMDB", keywords: searchTerms(msg("tmdb api key catalogue")) },
      // Tabs for these only exist once the service is on, so ⌘K lands on the
      // overview card where it can be enabled or configured.
      {
        key: "trakt",
        href: "/services/overview#trakt",
        title: "Trakt",
        keywords: searchTerms(msg("sync history")),
      },
      {
        key: "simkl",
        href: "/services/overview#simkl",
        title: "Simkl",
        keywords: searchTerms(msg("simkl sync history")),
      },
      {
        key: "mdblist",
        href: "/services/overview#mdblist",
        title: "MDBList",
        keywords: searchTerms(msg("mdblist sync history")),
      },
      {
        key: "oidc",
        href: "/services/overview#oidc",
        title: t("Single sign-on"),
        keywords: searchTerms(msg("oidc sso openid pocket id authelia authentik keycloak login")),
      },
      {
        key: "llm",
        href: "/services/overview#llm",
        title: t("Language model"),
        keywords: searchTerms(msg("llm ollama openai recaps search")),
      },
      {
        key: "subtitle-providers",
        href: settingHref("/services/overview", "subtitle-providers"),
        title: t("Subtitle providers"),
        keywords: searchTerms(
          msg("subtitles providers opensubtitles subdl subsource stremio add-on plugin"),
        ),
      },
      {
        key: "opensubtitles",
        href: "/services/overview#opensubtitles",
        title: "OpenSubtitles",
        keywords: searchTerms(msg("subtitles")),
      },
      {
        key: "subdl",
        href: "/services/overview#subdl",
        title: "SubDL",
        keywords: searchTerms(msg("subtitles subdl")),
      },
      {
        key: "subsource",
        href: "/services/overview#subsource",
        title: "SubSource",
        keywords: searchTerms(msg("subtitles subsource")),
      },
      {
        key: "ratings",
        title: t("OMDb and MDBList"),
        keywords: searchTerms(msg("ratings omdb mdblist api key")),
      },
      {
        key: "introdb",
        title: "TheIntroDB",
        keywords: searchTerms(msg("introdb intro credits markers skip")),
      },
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
    href: "/maintenance/health",
    title: t("Health"),
    sections: [],
    keywords: searchTerms(msg("crash watcher heartbeat uptime outage local file test playback")),
  },
  {
    href: "/maintenance/logs",
    title: t("Logs"),
    sections: [],
    keywords: searchTerms(msg("errors debug support zip record a problem")),
  },
]);
/** Pages in registry order, with their sections, in the active language. */
export function pagesFrom(pages: Descriptor<PageMeta>[]): PageDef[] {
  return pages.map((p) => ({
    href: pageRoute(p),
    title: textOf(p.label),
    sections: (p.meta?.sections ?? []).map((s) => ({
      key: s.key,
      title: textOf(s.label),
      keywords: searchTextOf(s.keywords),
      href: s.href,
    })),
    keywords: searchTextOf(p.meta?.keywords),
  }));
}

export function destinationsFrom(pages: Descriptor<PageMeta>[]): Destination[] {
  return pagesFrom(pages).flatMap((p) => [
    { href: p.href, title: p.title, page: t("Page"), keywords: p.keywords, isPage: true },
    ...p.sections.map((s) => ({
      href: s.href ?? `${p.href}/${s.key}`,
      title: s.title,
      page: p.title,
      keywords: s.keywords,
    })),
  ]);
}

export function destinations(): Destination[] {
  return PAGES.flatMap((p) => [
    { href: p.href, title: p.title, page: t("Page"), keywords: p.keywords, isPage: true },
    ...p.sections.map((s) => ({
      href: s.href ?? `${p.href}/${s.key}`,
      title: s.title,
      page: p.title,
      keywords: s.keywords,
    })),
  ]);
}
