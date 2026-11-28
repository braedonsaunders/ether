import {
  Badge,
  Button,
  Input,
  SettingsSection,
  Spinner,
  Switch,
  toast,
} from "@braedonsaunders/appkit-ui";
import { AlertTriangle, ChevronDown, ChevronRight, RefreshCw } from "lucide-react";
import { useState } from "react";
import { Link } from "react-router-dom";
import { ErrorPanel } from "../../../components/feedback";
import { NumberInput, SecretInput } from "../../../components/fields";
import { SaveBar, useDraft } from "../../../components/SaveBar";
import { SettingsRow } from "../../../components/SettingsRow";
import { localized, t, tn, tx } from "../../../i18n";
import { apiFetch, errorMessage } from "../../../lib/api";
import { formatDateTime } from "../../../lib/format";
import { useAsync } from "../../../lib/hooks";

export type RSSFeedStatus = {
  configured: boolean;
  lastRead: number;
  imported: number;
  unmatchedTitles: number;
  problem?: string;
  error?: string;
};

/** `/discovery/settings`: watchlist alerts plus the language-model features. */
export type DiscoverySettings = {
  enabled: boolean;
  plex: boolean;
  plexFriendsRssUrl?: string;
  plexOwnerRssUrl?: string;
  plexFriendsStatus?: (RSSFeedStatus & { id: string; username: string })[];
  plexFriendsRssStatus?: RSSFeedStatus;
  plexOwnerRssStatus?: RSSFeedStatus;
  trakt: boolean;
  notify: boolean;
  prewarm: boolean;
  checkHours: number;
  naturalSearch: boolean;
  recaps: boolean;
  subtitleTranslate?: boolean;
  speechToText?: boolean;
  overviewTranslate?: boolean;
};

/**
 * Load and save the shared discovery settings object. Each caller edits only
 * its own fields; saving sends the whole object back, as the API expects.
 */
export function useDiscoverySettings() {
  const doc = useAsync(() => apiFetch<DiscoverySettings>("/discovery/settings"), []);
  const { draft, setDraft, dirty, reset } = useDraft(doc.data);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const save = async (toastTitle: string) => {
    if (!draft) return;
    setSaving(true);
    setError(null);
    try {
      // Several pages edit this document; re-read it and apply only the keys
      // this page changed so a stale page never undoes another page's save.
      const base = doc.data as Record<string, unknown> | undefined;
      const edited = draft as Record<string, unknown>;
      const changed = Object.fromEntries(
        Object.entries(edited).filter(
          ([key, value]) => JSON.stringify(value) !== JSON.stringify(base?.[key]),
        ),
      );
      const live = await apiFetch<DiscoverySettings>("/discovery/settings");
      const next = await apiFetch<DiscoverySettings>("/discovery/settings", {
        method: "PUT",
        body: JSON.stringify({ ...live, ...changed }),
      });
      doc.setData(next);
      toast.success(toastTitle);
    } catch (e) {
      setError(e);
    } finally {
      setSaving(false);
    }
  };
  const discard = () => {
    reset();
    setError(null);
  };
  return { doc, draft, setDraft, dirty, saving, error, save, discard };
}

const SOURCES = localized(
  () =>
    [
      {
        key: "plex",
        label: t("Plex watchlists"),
        hint: t(
          "Plex Home members only. A friend's server access does not include their watchlist.",
        ),
      },
      {
        key: "trakt",
        label: t("Trakt watchlists"),
        hint: t("Anyone who has connected Trakt, including friends."),
      },
    ] as const,
);

const RSS_PROBLEMS = localized<Record<string, string>>(() => ({
  auth: t("Plex rejected this feed URL. Generate a fresh RSS link in Plex settings."),
  rate_limited: t("Plex asked Ether to slow down. Ether will try again."),
  invalid_feed: t("The feed response was not valid RSS XML."),
  metadata: t("Catalogue lookup failed temporarily. Previous items were kept."),
  network: t("Plex RSS could not be reached. Ether will try again."),
}));

function FeedStatusLine({ status, sharedSet }: { status?: RSSFeedStatus; sharedSet: boolean }) {
  if (!status?.configured) return null;
  const when = status.lastRead
    ? formatDateTime(new Date(status.lastRead * 1000))
    : t("Not read yet");
  const imported = sharedSet
    ? tn(status.imported, "{count} title in set", "{count} titles in set")
    : tn(status.imported, "{count} item imported", "{count} items imported");
  const unmatched = tn(
    status.unmatchedTitles,
    "{count} unmatched title",
    "{count} unmatched titles",
  );
  const is429 = /\b429\b/.test(status.error ?? "");
  const normalizedProblem =
    status.problem === "upstream" && is429 ? "rate_limited" : status.problem;
  const problem =
    normalizedProblem && normalizedProblem !== "upstream"
      ? (RSS_PROBLEMS[normalizedProblem] ?? RSS_PROBLEMS.network)
      : "";
  const showDetails = Boolean(status.error) && normalizedProblem !== "rate_limited";
  return (
    <span className="mt-1.5 block space-y-1 text-xs text-fg-muted">
      <span className="block">
        {t("Last read: {when}", { when })} · {imported} · {unmatched}
      </span>
      {problem || showDetails ? (
        <span className="inline-flex items-center gap-1.5 text-warning">
          <AlertTriangle size={13} aria-hidden />
          {problem}
          {showDetails ? <span>{t("Details: {details}", { details: status.error })}</span> : null}
        </span>
      ) : null}
    </span>
  );
}

type FriendStatus = RSSFeedStatus & { id: string; username: string };

function isFriendRateLimited(f: FriendStatus): boolean {
  return f.problem === "rate_limited" || /\b429\b/.test(f.error ?? "");
}

function hasFriendIssue(f: FriendStatus): boolean {
  return Boolean(f.problem || f.error);
}

function PlexFriendsStatusRow({
  friends: rawFriends,
  enabled,
  onSyncFriends,
  syncingFriends,
}: {
  friends: FriendStatus[];
  enabled: boolean;
  onSyncFriends: () => void;
  syncingFriends: boolean;
}) {
  const [expanded, setExpanded] = useState(false);
  const [filter, setFilter] = useState<"all" | "issues" | "synced">("all");
  const [query, setQuery] = useState("");

  const friendsSummaryError = rawFriends.find((f) => f.id === "friends");
  const friends = rawFriends.filter((f) => f.id !== "friends");
  if (friends.length === 0 && !friendsSummaryError) return null;

  const rateLimitedCount = friends.filter(isFriendRateLimited).length;
  const issueCount = friends.filter(hasFriendIssue).length;
  const syncedCount = friends.filter((f) => f.lastRead > 0 && !hasFriendIssue(f)).length;
  const totalTitles = friends.reduce((sum, f) => sum + (f.imported || 0), 0);

  const q = query.trim().toLowerCase();
  const visible = friends.filter((f) => {
    if (filter === "issues" && !hasFriendIssue(f)) return false;
    if (filter === "synced" && (f.lastRead <= 0 || hasFriendIssue(f))) return false;
    if (q && !f.username.toLowerCase().includes(q)) return false;
    return true;
  });

  return (
    <div>
      <SettingsRow
        setting="plex-community-friends"
        title={t("Plex friends' watchlists")}
        description={
          <div className="flex flex-wrap items-center gap-2">
            {friends.length > 0 ? (
              <span>
                {t("{count} friends · {synced} synced · {titles} titles", {
                  count: friends.length,
                  synced: syncedCount,
                  titles: totalTitles,
                })}
              </span>
            ) : null}
            {rateLimitedCount > 0 ? (
              <Badge variant="warning">
                {t("{count} rate-limited (HTTP 429)", { count: rateLimitedCount })}
              </Badge>
            ) : null}
            {issueCount > rateLimitedCount ? (
              <Badge variant="warning">
                {t("{count} errors", { count: issueCount - rateLimitedCount })}
              </Badge>
            ) : null}
            {friendsSummaryError && isFriendRateLimited(friendsSummaryError) ? (
              <Badge variant="warning">{RSS_PROBLEMS.rate_limited}</Badge>
            ) : friendsSummaryError?.error ? (
              <span className="text-xs text-warning">{friendsSummaryError.error}</span>
            ) : null}
          </div>
        }
        control={
          <div className="flex flex-wrap items-center gap-2">
            <Button
              type="button"
              variant="outline"
              size="sm"
              disabled={!enabled || syncingFriends}
              onClick={onSyncFriends}
            >
              <RefreshCw size={14} className={syncingFriends ? "animate-spin" : undefined} />
              {t("Sync friends now")}
            </Button>
            {friends.length > 0 ? (
              <Button
                type="button"
                variant="outline"
                size="sm"
                aria-expanded={expanded}
                onClick={() => setExpanded((v) => !v)}
              >
                {expanded ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
                {expanded
                  ? t("Hide friends")
                  : t("Show friends ({count})", { count: friends.length })}
              </Button>
            ) : null}
          </div>
        }
      />
      {expanded && friends.length > 0 ? (
        <div className="border-t border-border bg-surface/40 px-5 py-3 space-y-2.5">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <Input
              aria-label={t("Filter Plex friends")}
              placeholder={t("Filter by username…")}
              className="h-8 w-full text-xs sm:w-64"
              value={query}
              onChange={(e) => setQuery(e.currentTarget.value)}
            />
            <div className="flex flex-wrap items-center gap-1">
              {(
                [
                  ["all", t("All ({count})", { count: friends.length })],
                  ["issues", t("Issues ({count})", { count: issueCount })],
                  ["synced", t("Synced ({count})", { count: syncedCount })],
                ] as const
              ).map(([key, label]) => (
                <Button
                  key={key}
                  type="button"
                  size="sm"
                  variant={filter === key ? "secondary" : "ghost"}
                  onClick={() => setFilter(key)}
                >
                  {label}
                </Button>
              ))}
            </div>
          </div>
          <div className="max-h-80 space-y-2 overflow-y-auto pr-1">
            {visible.length === 0 ? (
              <p className="py-2 text-xs text-fg-muted">{t("No matching friends.")}</p>
            ) : (
              visible.map((friend) => (
                <div
                  key={friend.id}
                  className="rounded-md border border-border bg-surface px-3 py-2"
                >
                  <div className="flex items-center justify-between gap-2">
                    <span className="text-xs font-medium text-fg">{friend.username}</span>
                    {isFriendRateLimited(friend) ? <Badge variant="warning">HTTP 429</Badge> : null}
                  </div>
                  <FeedStatusLine status={friend} sharedSet />
                </div>
              ))
            )}
          </div>
        </div>
      ) : null}
    </div>
  );
}

/** Server-wide watchlist monitoring, edited in place on Viewers. */
export function WatchlistAlerts() {
  const s = useDiscoverySettings();
  const [syncingRss, setSyncingRss] = useState(false);
  const [syncingFriends, setSyncingFriends] = useState(false);
  if (s.doc.error) return <ErrorPanel error={s.doc.error} onRetry={() => void s.doc.reload()} />;
  if (!s.draft)
    return (
      <div className="py-10 text-center">
        <Spinner size={20} label={t("Loading watchlist settings")} />
      </div>
    );
  const draft = s.draft;
  const setRssField = (field: "plexFriendsRssUrl" | "plexOwnerRssUrl", nextValue: string) => {
    const savedVal = s.doc.data?.[field];
    if (savedVal === undefined && nextValue === "") {
      const copy = { ...draft };
      delete copy[field];
      s.setDraft(copy);
      return;
    }
    s.setDraft({ ...draft, [field]: nextValue });
  };
  const readRssNow = async () => {
    setSyncingRss(true);
    try {
      const next = await apiFetch<DiscoverySettings>("/discovery/rss/sync", {
        method: "POST",
        body: "{}",
      });
      s.doc.setData(next);
      toast.success(t("Watchlist RSS feeds read"));
    } catch (e) {
      toast.error(errorMessage(e));
    } finally {
      setSyncingRss(false);
    }
  };
  const syncFriendsNow = async () => {
    setSyncingFriends(true);
    try {
      const next = await apiFetch<DiscoverySettings>("/discovery/friends/sync", {
        method: "POST",
        body: "{}",
      });
      s.doc.setData(next);
      toast.success(t("Syncing Plex friends' watchlists in the background"));
    } catch (e) {
      toast.error(errorMessage(e));
    } finally {
      setSyncingFriends(false);
    }
  };
  const hasSavedRss = Boolean(
    s.doc.data?.plexFriendsRssStatus?.configured || s.doc.data?.plexOwnerRssStatus?.configured,
  );
  return (
    <>
      <SettingsSection
        title={t("On Home")}
        description={t(
          "A streamable watchlist title shows on their Home. A release date alone is not enough.",
        )}
      >
        {null}
      </SettingsSection>
      <SettingsSection title={t("Sources")}>
        <SettingsRow
          title={
            <label htmlFor="watchlist-enabled" data-setting="watchlist-monitoring">
              {t("Watchlist monitoring")}
            </label>
          }
          description={t("Check viewers' watchlists for titles that have become available.")}
          control={
            <Switch
              id="watchlist-enabled"
              aria-label={t("Watchlist monitoring")}
              checked={draft.enabled}
              onChange={(e) => s.setDraft({ ...draft, enabled: e.currentTarget.checked })}
            />
          }
        />
        {SOURCES.map((o) => (
          <SettingsRow
            key={o.key}
            setting={`${o.key}-watchlists`}
            title={<label htmlFor={`watchlist-${o.key}`}>{o.label}</label>}
            description={o.hint}
            control={
              <Switch
                id={`watchlist-${o.key}`}
                aria-label={o.label}
                checked={draft[o.key]}
                disabled={!draft.enabled}
                onChange={(e) => s.setDraft({ ...draft, [o.key]: e.currentTarget.checked })}
              />
            }
          />
        ))}
        <SettingsRow
          setting="plex-friends-watchlist-rss"
          title={<label htmlFor="watchlist-plex-friends-rss">{t("Friends' Watchlist RSS")}</label>}
          description={
            <>
              {t(
                "Paste the “Your Friends' Watchlist” RSS URL from plex.tv Watchlist settings. Plex doesn't say which friend added what, so it shows as a shared “Your friends want to watch” row.",
              )}
              <FeedStatusLine status={s.doc.data?.plexFriendsRssStatus} sharedSet />
            </>
          }
          control={
            <div className="w-full sm:w-80">
              <SecretInput
                id="watchlist-plex-friends-rss"
                ariaLabel={t("Friends' Watchlist RSS")}
                disabled={!draft.enabled}
                placeholder="https://rss.plex.tv/…"
                value={draft.plexFriendsRssUrl ?? ""}
                onChange={(v) => setRssField("plexFriendsRssUrl", v)}
              />
            </div>
          }
        />
        <SettingsRow
          setting="plex-your-watchlist-rss"
          title={<label htmlFor="watchlist-plex-owner-rss">{t("Your Watchlist RSS")}</label>}
          description={
            <>
              {t("Optional “Your Watchlist” RSS URL from plex.tv for the owner's watchlist.")}
              <FeedStatusLine status={s.doc.data?.plexOwnerRssStatus} sharedSet={false} />
            </>
          }
          control={
            <div className="w-full sm:w-80">
              <SecretInput
                id="watchlist-plex-owner-rss"
                ariaLabel={t("Your Watchlist RSS")}
                disabled={!draft.enabled}
                placeholder="https://rss.plex.tv/…"
                value={draft.plexOwnerRssUrl ?? ""}
                onChange={(v) => setRssField("plexOwnerRssUrl", v)}
              />
            </div>
          }
        />
        {hasSavedRss && !s.dirty ? (
          <div className="flex flex-wrap items-center gap-2 px-5 py-3">
            <Button
              type="button"
              variant="outline"
              size="sm"
              disabled={!draft.enabled || syncingRss}
              onClick={() => void readRssNow()}
            >
              <RefreshCw size={14} /> {t("Read RSS feeds now")}
            </Button>
          </div>
        ) : null}
        <PlexFriendsStatusRow
          friends={s.doc.data?.plexFriendsStatus ?? []}
          enabled={draft.enabled && draft.plex}
          onSyncFriends={() => void syncFriendsNow()}
          syncingFriends={syncingFriends}
        />
        <SettingsRow
          title={
            <label htmlFor="watchlist-hours" data-setting="check-every">
              {t("Check every")}
            </label>
          }
          description={t("Minimum time between checks of the same watchlist.")}
          control={
            <span className="flex items-center gap-2">
              <NumberInput
                id="watchlist-hours"
                aria-label={t("Check every")}
                className="w-24"
                min={1}
                max={168}
                disabled={!draft.enabled}
                value={draft.checkHours}
                onChange={(n) => s.setDraft({ ...draft, checkHours: n })}
              />
              <span className="text-sm text-fg-muted">{t("hours")}</span>
            </span>
          }
        />
      </SettingsSection>
      <SettingsSection title={t("Alerts")}>
        <SettingsRow
          setting="tell-me-too"
          title={<label htmlFor="watchlist-notify">{t("Tell me too")}</label>}
          description={tx(
            "Send availability alerts to the channels you set up in {notifications}.",
            {
              notifications: (
                <Link to="/notifications/channels" className="font-medium underline">
                  {t("Notifications")}
                </Link>
              ),
            },
          )}
          control={
            <Switch
              id="watchlist-notify"
              aria-label={t("Tell me too")}
              checked={draft.notify}
              disabled={!draft.enabled}
              onChange={(e) => s.setDraft({ ...draft, notify: e.currentTarget.checked })}
            />
          }
        />
      </SettingsSection>
      <SaveBar
        dirty={s.dirty}
        saving={s.saving}
        error={s.error}
        onSave={() => s.save(t("Watchlist settings saved"))}
        onDiscard={s.discard}
      />
    </>
  );
}
