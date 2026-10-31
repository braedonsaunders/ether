import { getLocale, t, tn } from "../../i18n";
import {
  Badge,
  Button,
  EmptyState,
  FilterChips,
  ListNavProvider,
  PagedTable,
  SearchInput,
  SettingsSection,
  Spinner,
  Switch,
  toast,
  type ListSearchParams,
} from "@braedonsaunders/appkit-ui";
import { Film, Settings2 } from "lucide-react";
import { useMemo, useState } from "react";
import { useLocation, useNavigate } from "react-router-dom";
import { Artwork } from "../../components/Artwork";
import { ConfigSection, MoreOptions } from "../../components/ConfigSection";
import { ErrorPanel, MutationError } from "../../components/feedback";
import { Segmented } from "../../components/fields";
import { pagedTableLabels } from "../../i18n/appkit-labels";
import { api, apiFetch } from "../../lib/api";
import { formatNumber, relativeTime } from "../../lib/format";
import { useAsync, useInterval } from "../../lib/hooks";
import type { AvailabilityConfig } from "../../lib/types";
import { RELEASE_SEARCH } from "../settings/schema";
import { BoolRow, NumRow, RecordDrawer } from "./form";

interface Feed {
  id: string;
  name: string;
  supported: boolean;
  selected: boolean;
}
interface Status {
  feeds: Feed[];
  usage: Record<string, number>;
  day: string;
  effort?: string;
  effectiveFeedRequests?: number;
  effectivePollMinutes?: number;
  effectiveMaxPages?: number;
  overAllowance?: boolean;
}
interface NewRelease {
  key: string;
  title: string;
  year?: number;
  poster?: string;
  theatrical?: number;
  digital?: number;
  code: string;
  source?: string;
  nextCheck?: number;
  reason?: string;
}

type Effort = "gentle" | "normal" | "unlimited" | "custom";

const STATUSES = ["playable", "searching", "theatrical", "quality"] as const;

function queryMap(search: string): ListSearchParams {
  const out: ListSearchParams = {};
  for (const [key, value] of new URLSearchParams(search)) out[key] = value;
  return out;
}

function selected(params: ListSearchParams, key: string) {
  const raw = params[key];
  const value = Array.isArray(raw) ? raw[0] : raw;
  return !value || value === "all" ? "" : value;
}

function shownEffort(effort?: string): Effort {
  if (effort === "gentle" || effort === "unlimited" || effort === "custom") return effort;
  return "normal";
}

function day(unix?: number) {
  if (!unix) return "—";
  return new Date(unix * 1000).toLocaleDateString(getLocale(), { dateStyle: "medium" });
}

function statusName(code: string) {
  if (code === "playable") return t("Ready to play");
  if (code === "theatrical") return t("In theatres only");
  if (code === "quality") return t("Hidden by your quality profile");
  return t("Waiting for a release");
}

function statusLabel(row: NewRelease) {
  if (row.code !== "searching") return statusName(row.code);
  const when = row.nextCheck ? relativeTime(new Date(row.nextCheck * 1000)) : t("soon");
  return t("Waiting for a release · next check {when}", { when });
}

function sourceLabel(source?: string) {
  if (!source || source === "date") return t("Digital or physical release date");
  return source;
}

export function Availability() {
  const navigate = useNavigate();
  const location = useLocation();
  const params = queryMap(location.search);
  const statusFilter = selected(params, "status");
  const listNav = useMemo(
    () => ({
      pathname: location.pathname,
      search: location.search.replace(/^\?/, ""),
      replace: (href: string) => navigate(href, { replace: true }),
      push: (href: string) => navigate(href),
    }),
    [location.pathname, location.search, navigate],
  );
  const status = useAsync((signal) => apiFetch<Status>("/availability/", { signal }), []);
  const titles = useAsync(
    (signal) => apiFetch<NewRelease[]>("/availability/titles", { signal }),
    [],
  );
  const config = useAsync(() => api.getConfig(), []);
  const [draft, setDraft] = useState<AvailabilityConfig | null>(null);
  const [saving, setSaving] = useState(false);
  const [toggling, setToggling] = useState(false);
  const [saveError, setSaveError] = useState<unknown>(null);
  useInterval(() => {
    void status.reload();
    void titles.reload();
  }, 30000);
  const cfg = config.data?.availability as AvailabilityConfig | undefined;
  const s = status.data;
  const movies = titles.data;
  if (status.error || titles.error || config.error)
    return (
      <ErrorPanel
        error={status.error || titles.error || config.error}
        onRetry={() => {
          void status.reload();
          void titles.reload();
          void config.reload();
        }}
      />
    );
  if (!s || !cfg || !movies)
    return (
      <div className="py-12 text-center">
        <Spinner label={t("Loading new releases")} />
      </div>
    );

  const change = <K extends keyof AvailabilityConfig>(key: K, value: AvailabilityConfig[K]) =>
    setDraft((d) => (d ? { ...d, [key]: value } : d));
  const evidenceDays = draft?.movieEvidenceDays ?? 60;
  const daysInvalid = !Number.isInteger(evidenceDays) || evidenceDays < 1 || evidenceDays > 366;
  const save = async () => {
    if (!draft) return;
    setSaving(true);
    setSaveError(null);
    try {
      await api.putSection("availability", draft);
      await config.reload();
      await status.reload();
      setDraft(null);
      toast.success(t("Availability settings saved"));
    } catch (e) {
      setSaveError(e);
    } finally {
      setSaving(false);
    }
  };
  const setMonitor = async (on: boolean) => {
    setToggling(true);
    setSaveError(null);
    try {
      await api.putSection("availability", { ...cfg, monitor: on });
      await config.reload();
      await status.reload();
    } catch (e) {
      setSaveError(e);
    } finally {
      setToggling(false);
    }
  };
  const query = selected(params, "q").trim().toLowerCase();
  const rows = movies.filter((row) => {
    if (statusFilter && row.code !== statusFilter) return false;
    if (!query) return true;
    return `${row.title} ${row.year ?? ""} ${statusLabel(row)}`.toLowerCase().includes(query);
  });
  const counts = Object.fromEntries(
    STATUSES.map((code) => [code, movies.filter((row) => row.code === code).length]),
  );
  const watched = s.feeds.filter((f) => f.supported && f.selected).length;
  const cap = s.effectiveFeedRequests || cfg.feedRequests;
  const used = s.usage.feedRequests ?? 0;
  const effort = draft ? shownEffort(draft.effort) : "normal";
  const openSettings = () => {
    setSaveError(null);
    setDraft({
      ...structuredClone(cfg),
      movieEvidenceDays: cfg.movieEvidenceDays || 60,
      effort: shownEffort(cfg.effort ?? s.effort),
      feedRequests: s.effectiveFeedRequests || cfg.feedRequests,
      pollMinutes: s.effectivePollMinutes || cfg.pollMinutes,
      maxPages: s.effectiveMaxPages || cfg.maxPages,
    });
  };

  return (
    <ListNavProvider value={listNav}>
      <p className="text-sm text-fg-muted">
        {t("New movies appear as soon as a release you can play is found.")}
      </p>
      <div className="flex flex-wrap items-center justify-between gap-4">
        <label className="flex items-center gap-3 text-sm font-medium text-fg">
          <Switch
            checked={cfg.monitor}
            disabled={toggling}
            onChange={(e) => void setMonitor(e.currentTarget.checked)}
          />
          {t("Look for playable releases")}
        </label>
        <Button variant="outline" size="sm" onClick={openSettings}>
          <Settings2 size={14} />
          {t("Settings")}
        </Button>
      </div>
      {saveError ? <MutationError error={saveError} /> : null}
      <p className="text-sm text-fg">
        {cfg.monitor
          ? tn(
              watched,
              "Watching new uploads on {count} indexer",
              "Watching new uploads on {count} indexers",
              { count: watched },
            )
          : t("Release checks are off.")}
      </p>
      <div className="max-w-md rounded-xl border border-border bg-surface px-5 py-4">
        <p className="text-xs text-fg-muted">{t("Indexer requests today")}</p>
        <p className="mt-2 text-2xl font-semibold tabular-nums">
          {formatNumber(used)}
          <span className="text-sm font-normal text-fg-muted"> / {formatNumber(cap)}</span>
        </p>
        <progress
          aria-label={t("Indexer requests today")}
          value={used}
          max={Math.max(1, cap)}
          className="mt-3 h-1 w-full accent-current"
        />
        <p className="mt-2 text-xs text-fg-muted">
          {s.overAllowance
            ? t(
                "The allowance was lowered after these requests. New checks wait until midnight UTC.",
              )
            : t("Budget for {day} UTC", { day: s.day })}
        </p>
      </div>
      <div className="flex flex-wrap items-center gap-2">
        <SearchInput
          searchLabel={t("Search new releases")}
          placeholder={t("Find a movie…")}
          clearLabel={t("Clear search")}
        />
        <FilterChips
          basePath={location.pathname}
          currentParams={params}
          paramKey="status"
          label={t("Status")}
          allLabel={t("All statuses")}
          options={STATUSES.map((code) => ({
            value: code,
            label: statusName(code),
            count: counts[code],
          }))}
        />
      </div>
      {movies.length ? (
        <PagedTable
          rows={rows}
          rowKey={(row) => row.key}
          pageSize={25}
          empty={t("No new releases match this status.")}
          labels={pagedTableLabels({
            searchLabel: t("Search new releases"),
            searchPlaceholder: t("Find a movie…"),
          })}
          columns={[
            {
              key: "title",
              header: t("Movie"),
              search: (row) => `${row.title} ${row.year ?? ""}`,
              sortValue: (row) => row.title,
              cell: (row) => (
                <span className="flex items-center gap-3">
                  <Artwork src={row.poster} alt="" className="aspect-[2/3] w-10 shrink-0 rounded" />
                  <span>
                    <span className="block font-medium">{row.title}</span>
                    {row.year ? <span className="text-xs text-fg-muted">{row.year}</span> : null}
                  </span>
                </span>
              ),
            },
            {
              key: "theatrical",
              header: t("In theatres"),
              sortValue: (row) => row.theatrical ?? 0,
              cell: (row) => day(row.theatrical),
            },
            {
              key: "digital",
              header: t("Digital"),
              sortValue: (row) => row.digital ?? 0,
              cell: (row) => day(row.digital),
            },
            {
              key: "source",
              header: t("Evidence"),
              search: (row) => (row.code === "playable" ? sourceLabel(row.source) : ""),
              cell: (row) => (row.code === "playable" ? sourceLabel(row.source) : "—"),
            },
            {
              key: "status",
              header: t("Status"),
              search: (row) => statusLabel(row),
              sortValue: (row) => row.code,
              cell: (row) => (
                <Badge
                  variant={
                    row.code === "playable"
                      ? "success"
                      : row.code === "quality"
                        ? "destructive"
                        : row.code === "theatrical"
                          ? "secondary"
                          : "info"
                  }
                >
                  {statusLabel(row)}
                </Badge>
              ),
            },
          ]}
        />
      ) : (
        <EmptyState
          icon={<Film />}
          title={t("No new movies in this window")}
          description={t("New movies appear as soon as a release you can play is found.")}
        />
      )}
      <ConfigSection
        configKey="resolver"
        endpoint="resolver"
        groups={RELEASE_SEARCH}
        label={t("How Ether searches")}
      />
      <RecordDrawer
        open={draft !== null}
        title={t("New release settings")}
        description={t("Choose how often Ether asks your indexers about new movies.")}
        onClose={() => {
          if (!saving) setDraft(null);
        }}
        onSave={() => void save()}
        saving={saving}
        saveDisabled={daysInvalid}
        saveLabel={t("Save settings")}
        error={saveError}
      >
        {draft ? (
          <>
            <SettingsSection title={t("How hard to use your indexers")}>
              <div className="px-4 py-3">
                <Segmented
                  ariaLabel={t("How hard to use your indexers")}
                  value={effort}
                  onChange={(value) => change("effort", value)}
                  options={[
                    { value: "gentle", label: t("Gentle") },
                    { value: "normal", label: t("Normal") },
                    { value: "unlimited", label: t("Unlimited") },
                  ]}
                />
                {effort === "custom" ? (
                  <p className="mt-2 text-sm text-fg-muted">
                    {t("Using the numbers under More options.")}
                  </p>
                ) : null}
              </div>
            </SettingsSection>
            <SettingsSection title={t("Movie visibility")}>
              <BoolRow
                label={t("Require movie release evidence")}
                value={!draft.disableMovieEvidenceGate}
                onChange={(v) => change("disableMovieEvidenceGate", !v)}
              />
              <BoolRow
                label={t("Count a passed digital date")}
                value={!!draft.digitalDateEvidence}
                onChange={(v) => change("digitalDateEvidence", v)}
                help={t(
                  "A digital or physical date can show a movie before a file is found. Theatrical dates never do.",
                )}
              />
            </SettingsSection>
            <MoreOptions label={t("More options")}>
              <SettingsSection title={t("Daily allowances")}>
                <NumRow
                  label={t("Feed API requests")}
                  value={draft.feedRequests}
                  onChange={(v) => setDraft({ ...draft, feedRequests: v, effort: "custom" })}
                  min={0}
                  max={100000}
                />
                <NumRow
                  label={t("Check API requests")}
                  value={draft.checkRequests}
                  onChange={(v) => setDraft({ ...draft, checkRequests: v, effort: "custom" })}
                  min={0}
                  max={100000}
                />
                <NumRow
                  label={t("Source checks")}
                  value={draft.checks}
                  onChange={(v) => setDraft({ ...draft, checks: v, effort: "custom" })}
                  min={0}
                  max={100000}
                />
                <NumRow
                  label={t("Descriptor grabs")}
                  value={draft.grabs}
                  onChange={(v) => setDraft({ ...draft, grabs: v, effort: "custom" })}
                  min={0}
                  max={100000}
                />
                <NumRow
                  label={t("Validation data")}
                  value={draft.checkBytes / 1048576}
                  onChange={(v) =>
                    setDraft({ ...draft, checkBytes: v * 1048576, effort: "custom" })
                  }
                  min={0}
                  max={102400}
                  unit="MiB"
                />
                <NumRow
                  label={t("Reserve for playback")}
                  value={draft.reservePercent}
                  onChange={(v) => setDraft({ ...draft, reservePercent: v, effort: "custom" })}
                  min={0}
                  max={99}
                  unit="%"
                />
                <NumRow
                  label={t("Poll interval")}
                  value={draft.pollMinutes}
                  onChange={(v) => setDraft({ ...draft, pollMinutes: v, effort: "custom" })}
                  min={5}
                  max={1440}
                  unit={t("minutes")}
                />
                <NumRow
                  label={t("Maximum pages per sync")}
                  value={draft.maxPages}
                  onChange={(v) => setDraft({ ...draft, maxPages: v, effort: "custom" })}
                  min={1}
                  max={20}
                />
                <NumRow
                  label={t("Movie evidence window")}
                  value={draft.movieEvidenceDays ?? 60}
                  onChange={(v) => change("movieEvidenceDays", v)}
                  min={1}
                  max={366}
                  unit={t("days")}
                  error={
                    (draft.movieEvidenceDays ?? 60) < 1 || (draft.movieEvidenceDays ?? 60) > 366
                      ? t("Enter 1 to 366 days")
                      : undefined
                  }
                />
              </SettingsSection>
              <SettingsSection title={t("Participating feeds")}>
                <BoolRow
                  label={t("Use all supported feeds")}
                  value={!draft.indexerIds?.length}
                  onChange={(v) => change("indexerIds", v ? [] : ["__none__"])}
                />
                <BoolRow
                  label={t("Also walk older uploads")}
                  value={!!draft.catchUp}
                  onChange={(v) => change("catchUp", v)}
                  help={t("Optional. New movies do not wait for this.")}
                />
                {draft.indexerIds?.length
                  ? s.feeds
                      .filter((f) => f.supported)
                      .map((f) => (
                        <BoolRow
                          key={f.id}
                          label={f.name}
                          value={draft.indexerIds!.includes(f.id)}
                          onChange={(v) => {
                            const ids = draft.indexerIds!.filter(
                              (id) => id !== f.id && id !== "__none__",
                            );
                            change(
                              "indexerIds",
                              v ? [...ids, f.id] : ids.length ? ids : ["__none__"],
                            );
                          }}
                        />
                      ))
                  : null}
              </SettingsSection>
            </MoreOptions>
          </>
        ) : null}
      </RecordDrawer>
    </ListNavProvider>
  );
}
