import {
  Badge,
  Button,
  EmptyState,
  Input,
  PagedTable,
  Select,
  confirmDialog,
  promptDialog,
  toast,
} from "@braedonsaunders/appkit-ui";
import { Ban, HeartPulse, Pin as PinIcon, PinOff, RotateCcw, SearchX, Undo2 } from "lucide-react";
import { useId, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { ActionMenu } from "../../components/ActionMenu";
import { ErrorPanel, LoadingInline, LoadingRows } from "../../components/feedback";
import { Field } from "../../components/fields";
import { Section } from "../../components/layout";
import { TitleSearch } from "../../components/pickers";
import { t, tn, tx } from "../../i18n";
import { errorMessage } from "../../lib/api";
import type { TitleCard } from "../../lib/contract";
import { formatBytes, formatNanos, relativeTime } from "../../lib/format";
import { useAsync } from "../../lib/hooks";
import {
  PROFILE_DOWNLOAD,
  PROFILE_NONE,
  ageDays,
  qualityLabel,
  rangeLabel,
  releaseConsole,
  type ChildTitle,
  type ConsoleRelease,
  type ConsoleSearch,
  type HealthResult,
} from "../../lib/releases";
import { RecordDrawer } from "../sources/form";
import { pagedTableLabels } from "../../i18n/appkit-labels";
import { PinStatusBadge } from "./Pinned";

const pad = (n: number | undefined) => String(n ?? 0).padStart(2, "0");

/** Pick a movie, or a show then a season then an episode. */
export function TitlePicker({ onPick }: { onPick: (key: string, name: string) => void }) {
  const id = useId();
  const [show, setShow] = useState<TitleCard | null>(null);
  const [season, setSeason] = useState("");
  const seasons = useAsync(
    (signal) => (show ? releaseConsole.children(show.key, signal) : Promise.resolve([])),
    [show?.key],
  );
  const episodes = useAsync(
    (signal) => (season ? releaseConsole.children(season, signal) : Promise.resolve([])),
    [season],
  );
  const seasonList = (seasons.data ?? []).filter((s) => s.type !== "episode");
  const episodeList = episodes.data ?? [];

  return (
    <div className="space-y-3">
      <TitleSearch
        placeholder={t("Find a movie or show…")}
        onPick={(title) => {
          if (title.type === "show") {
            setShow(title);
            setSeason("");
            return;
          }
          setShow(null);
          onPick(title.key, title.year ? `${title.title} (${title.year})` : title.title);
        }}
      />
      {show ? (
        <div className="flex flex-wrap items-end gap-3" role="group" aria-label={t("Episode")}>
          <p className="w-full text-sm font-medium text-fg">{show.title}</p>
          <Field label={t("Season")} htmlFor={`${id}-season`} className="w-48">
            <Select
              id={`${id}-season`}
              aria-label={t("Season")}
              value={season}
              onChange={(e) => setSeason(e.target.value)}
              disabled={!seasons.data}
            >
              <option value="">{seasons.data ? t("Choose a season") : t("Loading…")}</option>
              {seasonList.map((s: ChildTitle) => (
                <option key={s.key} value={s.key}>
                  {s.title || t("Season {n}", { n: s.season ?? 0 })}
                </option>
              ))}
            </Select>
          </Field>
          <Field label={t("Episode")} htmlFor={`${id}-episode`} className="min-w-64 flex-1">
            <Select
              id={`${id}-episode`}
              aria-label={t("Episode")}
              value=""
              disabled={!season || !episodes.data}
              onChange={(e) => {
                const ep = episodeList.find((x) => x.key === e.target.value);
                if (ep)
                  onPick(
                    ep.key,
                    `${show.title} S${pad(ep.season)}E${pad(ep.episode)}${ep.title ? ` · ${ep.title}` : ""}`,
                  );
              }}
            >
              <option value="">{t("Choose an episode")}</option>
              {episodeList.map((ep) => (
                <option key={ep.key} value={ep.key}>
                  {ep.pinned
                    ? t("{episode} (pinned)", { episode: `E${pad(ep.episode)} ${ep.title}` })
                    : `E${pad(ep.episode)} ${ep.title}`}
                </option>
              ))}
            </Select>
          </Field>
          {seasons.error ? <ErrorPanel error={seasons.error} className="w-full" /> : null}
          {episodes.error ? <ErrorPanel error={episodes.error} className="w-full" /> : null}
        </div>
      ) : null}
    </div>
  );
}

export function HealthBadge({ health }: { health?: HealthResult | null }) {
  if (!health) return <span className="text-xs text-fg-subtle">{t("Not checked")}</span>;
  if (health.source === "blocked")
    return (
      <span>
        <Badge variant="destructive">{t("Blocked")}</Badge>
      </span>
    );
  return (
    <span>
      {health.healthy ? (
        <Badge variant="success">{t("Healthy")}</Badge>
      ) : health.blocked ? (
        <Badge variant="destructive">{t("Failed")}</Badge>
      ) : (
        <Badge variant="warning">{t("Check failed")}</Badge>
      )}
      <span className="mt-1 block text-[11px] text-fg-subtle">
        {health.source === "verified"
          ? t("Verified {when}", { when: relativeTime(health.checkedAt) })
          : t("Checked {when}", { when: relativeTime(health.checkedAt) })}
      </span>
      {health.reason && !health.healthy ? (
        <span className="block text-[11px] text-fg-muted">{health.reason}</span>
      ) : null}
    </span>
  );
}

function verdict(r: ConsoleRelease) {
  if (r.rejected)
    return (
      <span>
        <Badge variant="destructive">{t("Rejected")}</Badge>
        {r.reason ? <span className="mt-1 block text-xs text-fg-muted">{r.reason}</span> : null}
      </span>
    );
  return (
    <span>
      <Badge variant={r.winner ? "success" : "secondary"}>
        {r.winner ? t("Plays first") : t("Fallback #{rank}", { rank: r.rank ?? "?" })}
      </Badge>
      {r.route ? <span className="mt-1 block text-[11px] text-fg-subtle">{r.route}</span> : null}
      {r.compatibility ? (
        <span className="mt-1 block text-[11px] text-fg-subtle">{r.compatibility}</span>
      ) : null}
    </span>
  );
}

/**
 * Find a release: every release for one title, ranked exactly as playback
 * ranks them, with pin, block and health-check actions. Lives on the Pinned
 * page; onPinsChanged lets that page refresh its list after a pin changes.
 */
export function ReleaseSearch({ onPinsChanged }: { onPinsChanged?: () => void }) {
  const id = useId();
  const [params, setParams] = useSearchParams();
  const key = params.get("key") ?? "";
  const titleName = params.get("title") ?? "";
  const [profile, setProfile] = useState("");
  const profiles = useAsync(() => releaseConsole.profiles(), []);
  const result = useAsync<ConsoleSearch | null>(
    (signal) => (key ? releaseConsole.search(key, { profile }, signal) : Promise.resolve(null)),
    [key, profile],
  );
  const [busy, setBusy] = useState(false);
  const [pinning, setPinning] = useState<ConsoleRelease | null>(null);
  const [note, setNote] = useState("");
  const [pinError, setPinError] = useState<unknown>(null);
  const [pinSaving, setPinSaving] = useState(false);

  const data = result.data && result.data.titleKey === key ? result.data : null;
  const rows = data?.releases ?? [];
  const name = data?.titleName || titleName || key;

  const act = async (fn: () => Promise<unknown>, done: string) => {
    setBusy(true);
    try {
      await fn();
      toast.success(done);
      await result.reload();
    } catch (e) {
      toast.error(t("That didn't work"), { description: errorMessage(e) });
    } finally {
      setBusy(false);
    }
  };

  const searchAgain = async () => {
    setBusy(true);
    try {
      await releaseConsole.forget(key);
      result.setData(await releaseConsole.search(key, { profile, fresh: true }));
      toast.success(t("Searched the indexers again"));
    } catch (e) {
      toast.error(t("Search failed"), { description: errorMessage(e) });
    } finally {
      setBusy(false);
    }
  };

  const block = async (r: ConsoleRelease) => {
    const reason = await promptDialog({
      title: t("Block this release?"),
      label: t("Reason"),
      placeholder: t("e.g. wrong language, bad encode"),
      confirmLabel: t("Block"),
    });
    if (reason === null) return;
    await act(
      () =>
        releaseConsole.block({
          releaseId: r.release.id,
          key,
          reason,
          name: r.release.name,
          indexer: r.release.indexer,
        }),
      t("Release blocked"),
    );
  };

  const unpin = async () => {
    const ok = await confirmDialog({
      title: t("Unpin this title?"),
      message: t("Ether goes back to picking the best release automatically."),
      confirmLabel: t("Unpin"),
    });
    if (ok) {
      await act(() => releaseConsole.unpin(key), t("Pin removed"));
      onPinsChanged?.();
    }
  };

  const check = async (r: ConsoleRelease) => {
    setBusy(true);
    try {
      const h = await releaseConsole.checkHealth(r.release.id, key);
      if (h.healthy) toast.success(t("Release is healthy"));
      else
        toast.error(h.blocked ? t("Release failed and was blocked") : t("Health check failed"), {
          description: h.reason,
        });
      await result.reload();
    } catch (e) {
      toast.error(t("Health check failed"), { description: errorMessage(e) });
    } finally {
      setBusy(false);
    }
  };

  const savePin = async () => {
    if (!pinning) return;
    setPinSaving(true);
    setPinError(null);
    try {
      await releaseConsole.pin(key, pinning.release.id, note);
      toast.success(t("Release pinned"));
      setPinning(null);
      await result.reload();
      onPinsChanged?.();
    } catch (e) {
      setPinError(e);
    } finally {
      setPinSaving(false);
    }
  };

  const def = profiles.data?.default;

  return (
    <>
      <Section
        title={t("Find a release")}
        description={t(
          "Search the indexers for one movie or episode and see every release, scored and judged exactly as playback would. Pin the one you want, or block a bad one.",
        )}
      >
        <TitlePicker onPick={(k, n) => setParams({ key: k, title: n })} />
        <div className="flex flex-wrap items-end gap-3">
          <Field label={t("Quality profile")} htmlFor={`${id}-profile`} className="w-64">
            <Select
              id={`${id}-profile`}
              aria-label={t("Quality profile")}
              value={profile}
              onChange={(e) => setProfile(e.target.value)}
            >
              <option value="">
                {def ? t("Default ({profile})", { profile: def }) : t("Default")}
              </option>
              {(profiles.data?.profiles ?? []).map((p) => (
                <option key={p} value={p}>
                  {p}
                </option>
              ))}
              <option value={PROFILE_NONE}>{t("No profile limits")}</option>
              <option value={PROFILE_DOWNLOAD}>{t("Offline download")}</option>
            </Select>
          </Field>
        </div>
      </Section>

      {!key ? null : (
        <Section
          title={t("Releases for {title}", { title: name })}
          description={
            data
              ? [
                  data.cached
                    ? t("Cached results from {when}", { when: relativeTime(data.searchedAt) })
                    : t("Fresh results from {when}", { when: relativeTime(data.searchedAt) }),
                  t("profile {profile}", { profile: data.profile }),
                  data.cached ? "" : t("took {duration}", { duration: formatNanos(data.duration) }),
                ]
                  .filter(Boolean)
                  .join(" · ")
              : undefined
          }
          actions={
            <>
              {busy ? <LoadingInline label={t("Working…")} /> : null}
              <Button
                variant="outline"
                size="sm"
                disabled={busy}
                onClick={() => void searchAgain()}
              >
                <RotateCcw size={14} /> {t("Search again")}
              </Button>
            </>
          }
        >
          {data?.pin ? (
            <div className="flex flex-wrap items-start justify-between gap-3 rounded-lg border border-border bg-bg-subtle px-4 py-3">
              <div className="min-w-0 space-y-1">
                <p className="flex flex-wrap items-center gap-2 text-sm font-medium text-fg">
                  <PinIcon size={14} aria-hidden /> {t("Pinned release")}
                  <PinStatusBadge pin={data.pin} />
                </p>
                <p className="break-all font-mono text-xs text-fg-muted">{data.pin.release.name}</p>
                {data.pinNote ? <p className="text-xs text-warning">{data.pinNote}</p> : null}
                {data.pin.note ? <p className="text-xs text-fg-muted">{data.pin.note}</p> : null}
              </div>
              <Button variant="outline" size="sm" disabled={busy} onClick={() => void unpin()}>
                <PinOff size={14} /> {t("Unpin")}
              </Button>
            </div>
          ) : null}
          {result.error ? (
            <ErrorPanel error={result.error} onRetry={() => void result.reload()} />
          ) : !data ? (
            <LoadingRows rows={5} />
          ) : (
            <>
              <p className="text-sm text-fg-muted">
                {data.winner
                  ? tx(
                      "Ether would try {release} first, then fall back in rank order if its health check fails.",
                      {
                        release: (
                          <span className="break-all font-mono text-xs text-fg">
                            {rows.find((r) => r.release.id === data.winner)?.release.name}
                          </span>
                        ),
                      },
                    )
                  : t("No release is acceptable for this profile.")}{" "}
                <Link
                  className="text-primary underline"
                  to={`/releases/history?key=${encodeURIComponent(key)}&title=${encodeURIComponent(name)}`}
                >
                  {t("Past decisions")}
                </Link>
              </p>
              <PagedTable<ConsoleRelease>
                rows={rows}
                rowKey={(r) => r.release.id}
                searchable
                pageSize={25}
                labels={pagedTableLabels({
                  searchLabel: t("Filter releases"),
                  searchPlaceholder: t("Filter by name, indexer or reason…"),
                })}
                empty={
                  <EmptyState
                    icon={<SearchX />}
                    title={t("No releases found")}
                    description={t(
                      "The indexers returned nothing for this title. Try Search again later.",
                    )}
                  />
                }
                defaultSort={{ key: "rank", dir: "asc" }}
                columns={[
                  {
                    key: "release",
                    header: t("Release"),
                    search: (r) =>
                      `${r.release.name} ${r.release.indexer} ${r.reason ?? ""} ${(r.customFormats ?? []).map((f) => f.name).join(" ")}`,
                    sortValue: (r) => r.release.name,
                    cell: (r) => {
                      const age = ageDays(r.release.pubDate);
                      return (
                        <span className="block min-w-0">
                          <span className="block break-all font-mono text-xs text-fg">
                            {r.release.name}
                          </span>
                          <span className="text-xs text-fg-muted">
                            {r.release.indexer}
                            {age !== null
                              ? ` · ${tn(age, "{count} day old", "{count} days old")}`
                              : ""}
                            {r.release.grabs
                              ? ` · ${tn(r.release.grabs, "{count} grab", "{count} grabs")}`
                              : ""}
                          </span>
                          {r.pinned || r.blocked ? (
                            <span className="mt-1 flex flex-wrap gap-1">
                              {r.pinned ? (
                                <Badge variant="info">
                                  <PinIcon size={11} /> {t("Pinned")}
                                </Badge>
                              ) : null}
                              {r.blocked ? (
                                <Badge variant="destructive" title={r.blockReason}>
                                  <Ban size={11} /> {t("Blocked")}
                                </Badge>
                              ) : null}
                            </span>
                          ) : null}
                        </span>
                      );
                    },
                  },
                  {
                    key: "quality",
                    header: t("Quality"),
                    sortValue: (r) => r.release.info?.height ?? 0,
                    cell: (r) => (
                      <span className="block text-xs">
                        <span className="block text-fg">{qualityLabel(r.release.info)}</span>
                        {rangeLabel(r.release.info) || r.release.info?.edition ? (
                          <span className="mt-1 flex flex-wrap gap-1">
                            {rangeLabel(r.release.info) ? (
                              <Badge variant="outline">{rangeLabel(r.release.info)}</Badge>
                            ) : null}
                            {r.release.info?.edition ? (
                              <Badge variant="outline">{r.release.info.edition}</Badge>
                            ) : null}
                          </span>
                        ) : null}
                      </span>
                    ),
                  },
                  {
                    key: "size",
                    header: t("Size"),
                    align: "right",
                    sortValue: (r) => r.release.size,
                    cell: (r) => (
                      <span className="tabular-nums">{formatBytes(r.release.size)}</span>
                    ),
                  },
                  {
                    key: "score",
                    header: t("Score"),
                    align: "right",
                    sortValue: (r) => r.score,
                    cell: (r) => (
                      <span className="block">
                        <span className="tabular-nums">{r.score.toFixed(0)}</span>
                        {(r.customFormats ?? []).length ? (
                          <span className="mt-1 flex flex-wrap justify-end gap-1">
                            {(r.customFormats ?? []).map((f) => (
                              <Badge key={f.id} variant="outline">
                                {f.name} {f.score > 0 ? `+${f.score}` : f.score}
                              </Badge>
                            ))}
                          </span>
                        ) : null}
                      </span>
                    ),
                  },
                  {
                    key: "rank",
                    header: t("Verdict"),
                    sortValue: (r) => (r.rank ? r.rank : 100000 - r.score),
                    cell: verdict,
                  },
                  {
                    key: "health",
                    header: t("Health"),
                    cell: (r) => <HealthBadge health={r.health} />,
                  },
                  {
                    key: "actions",
                    header: <span className="sr-only">{t("Actions")}</span>,
                    align: "right",
                    cell: (r) => (
                      <ActionMenu
                        label={t("Actions for {release}", { release: r.release.name })}
                        items={[
                          r.pinned
                            ? {
                                key: "unpin",
                                label: t("Unpin"),
                                icon: PinOff,
                                onSelect: () => void unpin(),
                                disabled: busy,
                              }
                            : {
                                key: "pin",
                                label: t("Pin to this title"),
                                icon: PinIcon,
                                onSelect: () => {
                                  setNote("");
                                  setPinError(null);
                                  setPinning(r);
                                },
                                disabled: busy || r.blocked,
                              },
                          {
                            key: "health",
                            label: t("Check health"),
                            icon: HeartPulse,
                            onSelect: () => void check(r),
                            disabled: busy,
                          },
                          r.blocked
                            ? {
                                key: "unblock",
                                label: t("Unblock"),
                                icon: Undo2,
                                onSelect: () =>
                                  void act(
                                    () => releaseConsole.unblock(r.release.id),
                                    t("Release unblocked"),
                                  ),
                                disabled: busy,
                              }
                            : {
                                key: "block",
                                label: t("Block…"),
                                icon: Ban,
                                danger: true,
                                onSelect: () => void block(r),
                                disabled: busy,
                              },
                        ]}
                      />
                    ),
                  },
                ]}
              />
            </>
          )}
        </Section>
      )}

      <RecordDrawer
        open={pinning !== null}
        title={t("Pin release")}
        description={name}
        onClose={() => setPinning(null)}
        onSave={() => void savePin()}
        saving={pinSaving}
        saveLabel={t("Pin release")}
        error={pinError}
      >
        {pinning ? (
          <div className="space-y-4">
            <div className="space-y-1 rounded-lg border border-border bg-bg-subtle px-4 py-3">
              <p className="break-all font-mono text-xs text-fg">{pinning.release.name}</p>
              <p className="text-xs text-fg-muted">
                {qualityLabel(pinning.release.info)} · {formatBytes(pinning.release.size)} ·{" "}
                {pinning.release.indexer}
              </p>
            </div>
            <p className="text-sm text-fg-muted">
              {t(
                "Every viewer gets this release first, whatever their quality profile, unless their app can't decode it, a viewer picks another version, or it fails its health check. Offline downloads keep their own profile.",
              )}
            </p>
            <Field
              label={t("Note")}
              htmlFor={`${id}-note`}
              hint={t("Optional: why you pinned it.")}
            >
              <Input
                id={`${id}-note`}
                value={note}
                maxLength={500}
                onChange={(e) => setNote(e.currentTarget.value)}
                placeholder={t("e.g. the only encode with correct colours")}
              />
            </Field>
          </div>
        ) : null}
      </RecordDrawer>
    </>
  );
}
