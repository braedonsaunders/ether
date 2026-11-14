import { t, tn } from "../../i18n";
import { formatDate } from "../../lib/format";
import type { CacheRetention } from "./Cache";

/** Human labels for the closed cache-cause vocabulary. */
export function causeLabel(code: string): string {
  switch (code) {
    case "playing":
      return t("Playing now");
    case "paused":
      return t("Paused");
    case "unfinished":
      return t("Unfinished");
    case "owner-pin":
      return t("Kept by you");
    case "shelf":
      return t("Shelf");
    case "next-episode":
      return t("Next episode");
    case "post-play":
      return t("Post-play");
    case "resume-warm":
      return t("Resume warm");
    case "detail-warm":
      return t("Detail warm");
    case "trailer-warm":
      return t("Trailer warm");
    case "browse-warm":
      return t("Browse warm");
    case "watchlist":
      return t("Watchlist");
    case "quality-check":
      return t("Checked");
    case "failover":
      return t("Failover");
    case "repair":
      return t("Repair");
    default:
      return t("Unexplained");
  }
}

/** One sentence explaining why a release is in the cache. */
export function causeDescription(code: string): string {
  switch (code) {
    case "playing":
      return t("Playback is using these bytes right now.");
    case "paused":
      return t("Playback is paused and may resume these bytes.");
    case "unfinished":
      return t("A saved viewing position keeps this release available.");
    case "owner-pin":
      return t("You pinned this release so it is not evicted.");
    case "shelf":
      return t("Chosen for the managed preload shelf.");
    case "next-episode":
      return t("Prepared before the next episode was played.");
    case "post-play":
      return t("Prepared for a post-play recommendation.");
    case "resume-warm":
      return t("Kept warm around a saved viewing position.");
    case "detail-warm":
      return t("Prepared when a detail page was opened.");
    case "trailer-warm":
      return t("Prepared while a trailer was playing.");
    case "browse-warm":
      return t("Prepared while a home or section row was on screen.");
    case "watchlist":
      return t("Fetched when the title arrived on a watchlist.");
    case "quality-check":
      return t("Written by a quality check or probe.");
    case "failover":
      return t("Fetched when playback switched to another release.");
    case "repair":
      return t("Fetched while repairing missing articles.");
    default:
      return t("No reason was recorded for this older entry.");
  }
}

/** Human labels for pin reasons the backend emits. */
export function pinLabel(code: string): string {
  switch (code) {
    case "owner":
      return t("Kept by you");
    case "playback:live":
      return t("Watching now");
    case "playback:inprogress":
      return t("Unfinished");
    case "preloaded":
      return t("Shelf");
    default:
      return t("Other");
  }
}

export function retentionLabel(retention?: CacheRetention): string {
  if (!retention) return t("Reclaimable anytime");
  switch (retention.kind) {
    case "pinned":
      return (retention.pins ?? []).map(pinLabel).join(" · ") || t("Kept by you");
    case "expiry": {
      const days = tn(retention.days ?? 0, "{count} inactive day", "{count} inactive days");
      return retention.until
        ? `${t("Expires {when}", {
            when: formatDate(retention.until),
          })} · ${days}`
        : days;
    }
    case "replay":
      return t("{hours} h replay credit", { hours: retention.hours ?? 0 });
    default:
      return t("Reclaimable anytime");
  }
}

export function shelfStateLabel(state?: string): string {
  switch (state) {
    case "cached":
      return t("Cached");
    case "failed":
      return t("Failed");
    default:
      return t("Queued");
  }
}

/** Localized fragments for a preload selection reason. */
export function selectionReason(codes?: string[], args?: Record<string, number>): string {
  const out = (codes ?? []).map((code) => {
    switch (code) {
      case "watchlist":
        return t("Watchlist");
      case "owner-pin":
        return t("Keep warm");
      case "catalogue-popularity":
        return t("Catalogue popularity");
      case "recent-release":
        return t("Recent release");
      case "household-viewing":
        return tn(
          args?.viewers ?? 0,
          "Recent viewing from {count} household viewer",
          "Recent viewing from {count} household viewers",
        );
      case "household-genres":
        return t("Matches household genre interests");
      case "next-in-sequence":
        return t("Next in a household viewing sequence");
      case "series-opening":
        return t("Series opening");
      case "continue-in-progress":
        return t("Continue an in-progress episode");
      default:
        return "";
    }
  });
  const text = out.filter(Boolean).join(" · ");
  return text || t("Selected for the local shelf");
}
