import { getLocale, localized, t } from "../../i18n";
import { Badge, Button, SettingsSection, Spinner, SubtabNav, Switch, toast } from "@braedonsaunders/appkit-ui";
import { Activity, Radio, RefreshCw, Settings2, ShieldCheck } from "lucide-react";
import { useState } from "react";
import { useNavigate, useParams } from "react-router-dom";
import { MoreOptions } from "../../components/ConfigSection";
import { ErrorPanel, MutationError } from "../../components/feedback";
import { Section } from "../../components/layout";
import type { AvailabilityConfig } from "../../lib/types";
import { api, apiFetch } from "../../lib/api";
import { formatNumber } from "../../lib/format";
import { useAsync, useInterval } from "../../lib/hooks";
import { BoolRow, NumRow, RecordDrawer } from "./form";

interface Feed {
  id: string;
  name: string;
  protocol: string;
  supported: boolean;
  reason?: string;
  selected: boolean;
  lastSync: number;
  gap: boolean;
  message: string;
  matched: number;
}
interface Observation {
  key: number;
  title: string;
  release: string;
  protocol: string;
  state: string;
  reason: string;
  firstSeen: number;
  verifiedAt: number;
}
interface Status {
  mode: string;
  message: string;
  feeds: Feed[];
  usage: Record<string, number>;
  counts: Record<string, number>;
  recent: Observation[];
  day: string;
}
const EVIDENCE_PAGE = 25;
const stamp = (n: number) =>
  n ? new Date(n * 1000).toLocaleString(getLocale()) : t("Not yet synced");
const labels: Record<string, string> = localized(() => ({
  observed: t("Discovered"),
  verified: t("Health checked"),
  stale: t("Needs refresh"),
  policy: t("Outside quality policy"),
  unknown: t("Unknown"),
  unavailable: t("Source unavailable"),
}));

export function Availability() {
  const { sub } = useParams();
  const navigate = useNavigate();
  const playTab = sub === "evidence" ? "evidence" : "feeds";
  const status = useAsync((signal) => apiFetch<Status>("/availability/", { signal }), []);
  const config = useAsync(() => api.getConfig(), []);
  const [draft, setDraft] = useState<AvailabilityConfig | null>(null);
  const [saving, setSaving] = useState(false);
  const [syncing, setSyncing] = useState(false);
  const [toggling, setToggling] = useState(false);
  const [evidencePage, setEvidencePage] = useState(0);
  const [error, setError] = useState<unknown>(null);
  const [saveError, setSaveError] = useState<unknown>(null);
  useInterval(() => void status.reload(), 30000);
  const cfg = config.data?.availability as AvailabilityConfig | undefined;
  const s = status.data;
  if (status.error || config.error)
    return (
      <ErrorPanel
        error={status.error || config.error}
        onRetry={() => {
          void status.reload();
          void config.reload();
        }}
      />
    );
  if (!s || !cfg)
    return (
      <div className="py-12 text-center">
        <Spinner label={t("Loading availability")} />
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
  const sync = async () => {
    setSyncing(true);
    setError(null);
    try {
      await apiFetch("/availability/sync", { method: "POST" });
      toast.success(t("Feed sync scheduled"), {
        description: t("The scheduler will pick it up within a minute, within your allowance."),
      });
      await status.reload();
    } catch (e) {
      setError(e);
    } finally {
      setSyncing(false);
    }
  };
  const activeFeeds = s.feeds.filter((f) => f.supported && f.selected).length;
  const dailyRequests = Math.ceil(1440 / Math.max(5, cfg.pollMinutes)) * activeFeeds;
  const evidencePages = Math.max(1, Math.ceil(s.recent.length / EVIDENCE_PAGE));
  const evidenceAt = Math.min(evidencePage, evidencePages - 1);
  const evidenceRows = s.recent.slice(
    evidenceAt * EVIDENCE_PAGE,
    evidenceAt * EVIDENCE_PAGE + EVIDENCE_PAGE,
  );
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
  return (
    <>
      <div className="flex flex-wrap items-center justify-between gap-4">
        <label className="flex items-center gap-3 text-sm font-medium text-fg">
          <Switch
            checked={cfg.monitor}
            disabled={toggling}
            onChange={(e) => void setMonitor(e.currentTarget.checked)}
          />
          {t("Watch release feeds")}
        </label>
        <div className="flex gap-2">
          <Button
            variant="outline"
            size="sm"
            disabled={!cfg.monitor || syncing}
            onClick={() => void sync()}
          >
            <RefreshCw size={14} />
            {syncing ? t("Scheduling…") : t("Sync feeds")}
          </Button>
          <Button
            variant="outline"
            size="sm"
            onClick={() => {
              setSaveError(null);
              setDraft({ ...structuredClone(cfg), movieEvidenceDays: cfg.movieEvidenceDays || 60 });
            }}
          >
            <Settings2 size={14} />
            {t("Settings")}
          </Button>
        </div>
      </div>
      {error ? <MutationError error={error} /> : null}
      {saveError ? <MutationError error={saveError} /> : null}
      {cfg.monitor && cfg.feedRequests < dailyRequests ? (
        <p className="text-sm text-danger" role="status">
          {t("The current allowance cannot sustain this polling interval for every feed.")}
        </p>
      ) : null}
      <div className="grid gap-3 sm:grid-cols-3">
        {[
          [Radio, t("Feed requests"), s.usage.feedRequests ?? 0, cfg.feedRequests],
          [Activity, t("Check API requests"), s.usage.checkRequests ?? 0, cfg.checkRequests],
          [ShieldCheck, t("Source checks"), s.usage.checks ?? 0, cfg.checks],
        ].map(([Icon, title, used, cap]) => {
          const Glyph = Icon as typeof Radio;
          return (
            <div
              key={String(title)}
              className="rounded-xl border border-border bg-surface px-5 py-4"
            >
              <div className="flex items-center gap-2 text-xs text-fg-muted">
                <Glyph size={15} />
                {String(title)}
              </div>
              <p className="mt-3 text-2xl font-semibold tabular-nums">
                {formatNumber(Number(used))}
                <span className="text-sm font-normal text-fg-muted">
                  {" "}
                  / {formatNumber(Number(cap))}
                </span>
              </p>
              <progress
                aria-label={String(title)}
                value={Number(used)}
                max={Math.max(1, Number(cap))}
                className="mt-3 h-1 w-full accent-current"
              />
              <p className="mt-2 text-xs text-fg-muted">
                {Number(cap) === 0
                  ? t("Disabled")
                  : t("Daily allowance · {day} UTC", { day: s.day })}
              </p>
            </div>
          );
        })}
      </div>
      <SubtabNav
        ariaLabel={t("Will it play sections")}
        active={playTab}
        onSelect={(key) => navigate(`/sources/availability/${key}`)}
        tabs={[
          { key: "feeds", label: t("Release feeds") },
          { key: "evidence", label: t("Recent source evidence") },
        ]}
      />
      {playTab === "feeds" ? (
      <Section title={t("Release feeds")}>
        {s.feeds.length ? (
          <ul className="divide-y divide-border">
            {s.feeds.map((f) => (
              <li key={f.id} className="flex flex-wrap items-start justify-between gap-3 py-3">
                <div className="min-w-0 flex-1">
                  <p className="text-sm font-medium">
                    {f.name}{" "}
                    <span className="font-normal text-fg-muted">· {f.protocol || "Usenet"}</span>
                  </p>
                  <p className="mt-1 text-xs text-fg-muted">
                    {f.reason || f.message || t("Ready to monitor")}
                  </p>
                  <p className="mt-1 text-xs text-fg-muted">{stamp(f.lastSync)}</p>
                </div>
                <Badge>
                  {!f.supported
                    ? t("Search only")
                    : !f.selected
                      ? t("Excluded")
                      : f.gap
                        ? t("Coverage gap")
                        : f.lastSync
                          ? t("Synced")
                          : t("Waiting")}
                </Badge>
              </li>
            ))}
          </ul>
        ) : (
          <p className="py-4 text-sm text-fg-muted">
            {t("Add a Newznab or Torznab indexer to discover recent releases.")}
          </p>
        )}
      </Section>
      ) : (
      <Section title={t("Recent source evidence")}>
        {s.recent.length ? (
          <>
          <ul className="divide-y divide-border">
            {evidenceRows.map((r, i) => (
              <li key={`${r.key}-${i}`} className="flex flex-wrap items-start gap-3 py-3">
                <div className="min-w-0 flex-1">
                  <p className="text-sm font-medium">{r.title}</p>
                  <p className="mt-1 break-all text-xs text-fg-muted">{r.release}</p>
                  <p className="mt-1 text-xs text-fg-muted">
                    {r.reason || t("Waiting for a selective check")} ·{" "}
                    {stamp(r.verifiedAt || r.firstSeen)}
                  </p>
                </div>
                <Badge>{labels[r.state] || r.state}</Badge>
              </li>
            ))}
          </ul>
          {s.recent.length > EVIDENCE_PAGE ? (
            <div className="mt-4 flex items-center justify-between gap-3">
              <p className="text-xs text-fg-muted">
                {t("{start}–{end} of {count}", {
                  start: evidenceAt * EVIDENCE_PAGE + 1,
                  end: Math.min(s.recent.length, evidenceAt * EVIDENCE_PAGE + EVIDENCE_PAGE),
                  count: s.recent.length,
                })}
              </p>
              <div className="flex gap-2">
                <Button
                  variant="outline"
                  size="sm"
                  disabled={evidenceAt === 0}
                  onClick={() => setEvidencePage(evidenceAt - 1)}
                >
                  {t("Previous")}
                </Button>
                <Button
                  variant="outline"
                  size="sm"
                  disabled={evidenceAt >= evidencePages - 1}
                  onClick={() => setEvidencePage(evidenceAt + 1)}
                >
                  {t("Next")}
                </Button>
              </div>
            </div>
          ) : null}
          </>
        ) : (
          <div className="py-8 text-center">
            <ShieldCheck className="mx-auto mb-3 text-fg-muted" size={28} />
            <p className="text-sm font-medium">{t("Evidence will appear here")}</p>
            <p className="mx-auto mt-2 max-w-lg text-sm text-fg-muted">
              {cfg.monitor
                ? t("Checked releases show up here after Ether watches a feed.")
                : t("Turn on Watch release feeds to start collecting this.")}
            </p>
          </div>
        )}
      </Section>
      )}
      <RecordDrawer
        open={draft !== null}
        title={t("Availability settings")}
        description={t(
          "This watches your search sites for new releases and can spot-check that a file is really there before someone presses Play. It does not change which account Ether streams from.",
        )}
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
          <SettingsSection
            title={t("Movie visibility")}
            description={t(
              "An eligible observed release unlocks a movie immediately; this is not a mandatory delay or a guarantee of quality or playback. Episodes retain their 24-hour policy.",
            )}
          >
            <BoolRow
              label={t("Require movie release evidence")}
              value={!draft.disableMovieEvidenceGate}
              onChange={(v) => change("disableMovieEvidenceGate", !v)}
            />
            <NumRow
              label={t("Movie evidence window")}
              value={draft.movieEvidenceDays ?? 60}
              onChange={(v) => change("movieEvidenceDays", v)}
              min={1}
              max={366}
              unit={t("days")}
              error={
                draft.movieEvidenceDays! < 1 || draft.movieEvidenceDays! > 366
                  ? t("Enter 1 to 366 days")
                  : undefined
              }
            />
          </SettingsSection>
          <SettingsSection title={t("Discovery & presentation")}>
            <BoolRow
              label={t("Monitor release feeds")}
              value={draft.monitor}
              onChange={(v) => change("monitor", v)}
              help={t(
                "Use your configured indexers automatically. No separate RSS URL. Requests count toward API budgets; catch-up is bounded.",
              )}
            />
            <BoolRow
              label={t("Check discovered sources")}
              value={draft.verify}
              onChange={(v) => change("verify", v)}
              help={t(
                "Spend a small allowance validating promising sources. Checks pause while playback is active.",
              )}
            />
            <BoolRow
              label={t("Use debrid for checks")}
              value={draft.debrid}
              onChange={(v) => change("debrid", v)}
              help={t(
                "Use supported cache checks and existing account files. Provider capabilities vary; unknown is never treated as absent.",
              )}
            />
            <BoolRow
              label={t("Prefer checked recommendations")}
              value={draft.preferVerified}
              onChange={(v) => change("preferVerified", v)}
              help={t(
                "Move fresh checked movies forward in discovery shelves. Visibility follows the evidence settings above.",
              )}
            />
            <BoolRow
              label={t("Show newly available shelves")}
              value={draft.newlyAvailable}
              onChange={(v) => change("newlyAvailable", v)}
            />
          </SettingsSection>
          <MoreOptions label={t("More options")}>
            <p className="mt-2 mb-3 px-4 text-sm text-fg-muted">
              {t(
                "Polling, which search sites to watch, and how much of each day's allowance this work may use. The defaults are safe.",
              )}
            </p>
            <SettingsSection
              title={t("Participating feeds")}
              description={t(
                "All supported enabled indexers are included by default. Search remains enabled for excluded feeds.",
              )}
            >
              <BoolRow
                label={t("Use all supported feeds")}
                value={!draft.indexerIds?.length}
                onChange={(v) => change("indexerIds", v ? [] : ["__none__"])}
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
            <SettingsSection
              title={t("Daily allowances")}
              description={t(
                "Shared across the server, reset at midnight UTC. A zero allowance disables that work. Normal playback retains its existing provider budgets.",
              )}
            >
              <NumRow
                label={t("Feed API requests")}
                value={draft.feedRequests}
                onChange={(v) => change("feedRequests", v)}
                min={0}
                max={100000}
              />
              <NumRow
                label={t("Check API requests")}
                value={draft.checkRequests}
                onChange={(v) => change("checkRequests", v)}
                min={0}
                max={100000}
                help={t("Indexer and debrid API calls used by source validation.")}
              />
              <NumRow
                label={t("Source checks")}
                value={draft.checks}
                onChange={(v) => change("checks", v)}
                min={0}
                max={100000}
              />
              <NumRow
                label={t("Descriptor grabs")}
                value={draft.grabs}
                onChange={(v) => change("grabs", v)}
                min={0}
                max={100000}
              />
              <NumRow
                label={t("Validation data")}
                value={draft.checkBytes / 1048576}
                onChange={(v) => change("checkBytes", v * 1048576)}
                min={0}
                max={102400}
                unit="MiB"
              />
              <NumRow
                label={t("Reserve for playback")}
                value={draft.reservePercent}
                onChange={(v) => change("reservePercent", v)}
                min={0}
                max={99}
                unit="%"
                help={t(
                  "Background requests stop before using this share of each indexer's configured daily cap. Set those caps on the Indexers tab.",
                )}
              />
              <NumRow
                label={t("Poll interval")}
                value={draft.pollMinutes}
                onChange={(v) => change("pollMinutes", v)}
                min={5}
                max={1440}
                unit={t("minutes")}
              />
              <NumRow
                label={t("Maximum pages per sync")}
                value={draft.maxPages}
                onChange={(v) => change("maxPages", v)}
                min={1}
                max={20}
                help={t(
                  "A page contains up to 100 releases. Unfinished catch-up resumes on later polls, within the daily allowance; fresh arrivals are checked first.",
                )}
              />
            </SettingsSection>
          </MoreOptions>
          </>
        ) : null}
      </RecordDrawer>
    </>
  );
}
