import { getLocale, localized, t, tn } from "../../i18n";
import { Badge, Button, SettingsSection, Spinner, toast } from "@braedonsaunders/appkit-ui";
import { Activity, Radio, RefreshCw, Settings2, ShieldCheck } from "lucide-react";
import { useState } from "react";
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
  const status = useAsync((signal) => apiFetch<Status>("/availability/", { signal }), []);
  const config = useAsync(() => api.getConfig(), []);
  const [draft, setDraft] = useState<AvailabilityConfig | null>(null);
  const [saving, setSaving] = useState(false);
  const [syncing, setSyncing] = useState(false);
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
    setDraft((d) => d && { ...d, [key]: value });
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
  return (
    <>
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div className="max-w-2xl">
          <div className="mb-2 flex items-center gap-2">
            <Radio size={19} className="text-accent" />
            <h2 className="text-lg font-semibold">{t("Availability intelligence")}</h2>
            <Badge>
              {s.mode === "off"
                ? t("Off")
                : s.mode === "checking"
                  ? cfg.monitor
                    ? t("Monitoring & checking")
                    : t("Checking")
                  : t("Monitoring")}
            </Badge>
          </div>
          <p className="text-sm text-fg-muted">{s.message}</p>
          <p className="mt-2 text-xs text-fg-muted">
            {cfg.disableMovieEvidenceGate
              ? t("Movie evidence requirement is off.")
              : tn(
                  cfg.movieEvidenceDays || 60,
                  "Movies need evidence for {count} day after release.",
                  "Movies need evidence for {count} days after release.",
                )}{" "}
            {t(
              "Episodes need evidence for 24 hours. Eligible observed releases unlock immediately, without a mandatory delay; evidence does not guarantee quality or playback.",
            )}
          </p>
        </div>
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
      <p className="text-xs text-fg-muted">
        {tn(
          activeFeeds,
          "At this interval, {count} feed needs approximately {requests} requests per day for fresh arrivals.",
          "At this interval, {count} feeds need approximately {requests} requests per day for fresh arrivals.",
          { requests: formatNumber(dailyRequests) },
        )}{" "}
        {t("Catch-up uses only the remaining allowance and saves its progress across restarts.")}{" "}
        {cfg.feedRequests < dailyRequests
          ? t("The current allowance cannot sustain this polling interval for every feed.")
          : t("Allow additional requests for busy feeds and downtime.")}
      </p>
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
      <Section
        title={t("Release feeds")}
        description={t(
          "One request discovers many releases. Ether matches them against local catalogue metadata without searching every title.",
        )}
      >
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
            {t(
              "Add a Newznab or Torznab indexer to discover recent releases. A debrid account alone does not provide a global release feed.",
            )}
          </p>
        )}
      </Section>
      <Section
        title={t("Recent source evidence")}
        description={t(
          "Health checks apply to a specific source and the owner's quality policy. They expire after six hours; they cannot guarantee every byte or every device.",
        )}
      >
        {s.recent.length ? (
          <ul className="divide-y divide-border">
            {s.recent.map((r, i) => (
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
        ) : (
          <div className="py-8 text-center">
            <ShieldCheck className="mx-auto mb-3 text-fg-muted" size={28} />
            <p className="text-sm font-medium">{t("Evidence will appear here")}</p>
            <p className="mx-auto mt-2 max-w-lg text-sm text-fg-muted">
              {t(
                "Enable release monitoring to follow new arrivals. Successful playback health checks also contribute, without extra indexer searches.",
              )}
            </p>
          </div>
        )}
      </Section>
      <p className="text-xs text-fg-muted">
        {t(
          "Today: {grabs} / {grabCap} Descriptor grabs · {used} / {cap} MiB reserved for source validation.",
          {
            grabs: formatNumber(s.usage.grabs ?? 0),
            grabCap: formatNumber(cfg.grabs),
            used: formatNumber(Math.ceil((s.usage.bytes ?? 0) / 1048576)),
            cap: formatNumber(Math.ceil(cfg.checkBytes / 1048576)),
          },
        )}{" "}
        {t("Reservations are conservative; actual transfers can be smaller.")}
      </p>
      <RecordDrawer
        open={draft !== null}
        title={t("Availability settings")}
        description={t("Configure movie visibility, polling and optional health checks.")}
        onClose={() => {
          if (!saving) setDraft(null);
        }}
        onSave={() => void save()}
        saving={saving}
        saveDisabled={
          !!draft &&
          (!Number.isInteger(draft.movieEvidenceDays) ||
            draft.movieEvidenceDays! < 1 ||
            draft.movieEvidenceDays! > 366)
        }
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
          </>
        ) : null}
      </RecordDrawer>
    </>
  );
}
