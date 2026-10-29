import { DashboardPanel } from "@braedonsaunders/appkit-dashboard/primitives";
import { Badge, Button } from "@braedonsaunders/appkit-ui";
import { ArrowUpRight, HardDrive, ShieldCheck, Sparkles, Zap } from "lucide-react";
import type { EtherConfig } from "../../lib/types";
import { t, tn } from "../../i18n";
import { formatBytes, formatPercent } from "../../lib/format";
import type { CacheSnapshot } from "./Cache";

export function CacheOverview({
  snapshot: s,
  policy: p,
  name,
  freeBytes,
  busy,
  onProfile,
  onPreloading,
  onExplore,
  onWarm,
}: {
  snapshot?: CacheSnapshot;
  policy: EtherConfig["cache"];
  name: string;
  freeBytes?: number;
  busy: boolean;
  onProfile: () => void;
  onPreloading: () => void;
  onExplore: () => void;
  onWarm: () => void;
}) {
  const used = s?.used ?? 0,
    budget = s?.budget ?? p.budgetBytes;
  const protectedBytes = Math.min(used, s?.protectedBytes ?? 0);
  const reads = (s?.metrics.hits ?? 0) + (s?.metrics.misses ?? 0);
  const percent = budget > 0 ? Math.min(100, (used / budget) * 100) : 0;
  const warming = p.mode !== "off" && !p.disableSpeculation;
  return (
    <div className="flex min-h-0 flex-col gap-4 cockpit:h-full">
      <div className="grid shrink-0 grid-cols-2 gap-3 lg:grid-cols-4">
        {[
          [
            t("Retained"),
            s ? formatBytes(used) : "—",
            s?.shared
              ? s.clustered
                ? tn(
                    s.releases.length,
                    "{count} release in the shared pool",
                    "{count} releases in the shared pool",
                  )
                : tn(s.releases.length, "{count} release on the NAS", "{count} releases on the NAS")
              : tn(
                  s?.releases.length ?? 0,
                  "{count} release on this node",
                  "{count} releases on this node",
                ),
          ],
          [
            t("Disk hit rate"),
            reads ? formatPercent(s!.metrics.hits / reads) : "—",
            reads
              ? tn(reads, "{count} disk lookup", "{count} disk lookups")
              : t("Waiting for playback"),
          ],
          [
            t("Served locally"),
            s ? formatBytes(s.metrics.servedBytes) : "—",
            s?.shared ? t("Served by this node from NAS") : t("Delivered from the disk cache"),
          ],
          [
            t("Space reclaimed"),
            s ? formatBytes(s.metrics.evictedBytes) : "—",
            t("Released by cache retention"),
          ],
        ].map(([label, value, hint]) => (
          <div key={label} className="rounded-xl border border-border bg-surface px-4 py-3.5">
            <p className="text-xs text-fg-muted">{label}</p>
            <p className="mt-1 text-2xl font-semibold tracking-tight tabular-nums">{value}</p>
            <p className="mt-1 text-[10px] text-fg-subtle">{hint}</p>
          </div>
        ))}
      </div>
      <div className="grid gap-4 lg:grid-cols-12 cockpit:min-h-0 cockpit:flex-1">
        <DashboardPanel
          title={t("Storage")}
          icon={<HardDrive size={16} />}
          className="lg:col-span-5 cockpit:min-h-0"
          bodyClassName="flex flex-col gap-4 cockpit:overflow-y-auto"
          actions={
            <button
              onClick={onExplore}
              className="text-xs font-medium text-primary hover:underline"
            >
              {t("Open explorer")}
            </button>
          }
        >
          <div className="flex flex-wrap items-center justify-center gap-6 py-2">
            <div
              role="img"
              aria-label={
                budget
                  ? t("{used} retained of {budget}", {
                      used: formatBytes(used),
                      budget: formatBytes(budget),
                    })
                  : t("{used} retained", { used: formatBytes(used) })
              }
              className="relative grid h-36 w-36 shrink-0 place-items-center"
            >
              <svg
                viewBox="0 0 160 160"
                className="absolute inset-0 h-full w-full -rotate-90"
                aria-hidden="true"
              >
                <circle
                  cx="80"
                  cy="80"
                  r="68"
                  fill="none"
                  stroke="currentColor"
                  strokeWidth="10"
                  className="text-primary/10"
                />
                <circle
                  cx="80"
                  cy="80"
                  r="68"
                  fill="none"
                  stroke="currentColor"
                  strokeWidth="10"
                  strokeLinecap="round"
                  strokeDasharray={`${percent * 4.273} 427.3`}
                  className="text-primary transition-all duration-700"
                />
              </svg>
              <div className="text-center">
                <p className="text-2xl font-semibold tabular-nums">
                  {budget > 0 ? formatPercent(percent / 100) : "—"}
                </p>
                <p className="text-[11px] text-fg-muted">
                  {p.mode === "off" ? t("writes off") : t("of budget")}
                </p>
              </div>
            </div>
            <div className="min-w-0 space-y-3 text-sm">
              <div>
                <p className="text-xs text-fg-muted">{t("Media budget")}</p>
                <p className="font-semibold">
                  {p.mode === "off"
                    ? t("Pass-through")
                    : budget
                      ? formatBytes(budget)
                      : t("Unlimited")}
                </p>
              </div>
              <div>
                <p className="text-xs text-fg-muted">{t("Free on disk")}</p>
                <p className="font-semibold">
                  {freeBytes === undefined ? "—" : formatBytes(freeBytes)}
                </p>
              </div>
            </div>
          </div>
          <div className="space-y-2 border-t border-border pt-4">
            <div className="flex h-2 overflow-hidden rounded-full bg-primary/10" aria-hidden="true">
              <span
                className="bg-primary"
                style={{ width: `${used ? (protectedBytes / used) * 100 : 0}%` }}
              />
              <span
                className="bg-primary/35"
                style={{ width: `${used ? ((used - protectedBytes) / used) * 100 : 0}%` }}
              />
            </div>
            <div className="flex justify-between gap-3 text-[11px] text-fg-muted">
              <span>
                <span className="mr-1 inline-block h-1.5 w-1.5 rounded-full bg-primary" />
                {t("{size} protected", { size: formatBytes(protectedBytes) })}
              </span>
              <span>{t("{size} reclaimable", { size: formatBytes(used - protectedBytes) })}</span>
            </div>
          </div>
          <p
            className="mt-auto truncate rounded-md bg-bg-subtle px-2.5 py-2 text-[10px] text-fg-subtle"
            title={s?.directory ?? p.dir}
          >
            {s?.directory ?? p.dir}
          </p>
        </DashboardPanel>
        <DashboardPanel
          title={t("Playback readiness")}
          icon={<Zap size={16} />}
          className="lg:col-span-4 cockpit:min-h-0"
          bodyClassName="flex flex-col gap-4 cockpit:overflow-y-auto"
          actions={
            <Badge variant={warming ? "success" : "secondary"}>
              {warming ? t("Enabled") : t("On demand")}
            </Badge>
          }
        >
          <div className="grid grid-cols-2 gap-3 rounded-lg bg-primary/5 p-3">
            <div>
              <p className="text-xl font-semibold tabular-nums">{s?.prefetch.running ?? 0}</p>
              <p className="text-[11px] text-fg-muted">{t("Warming now")}</p>
            </div>
            <div>
              <p className="text-xl font-semibold tabular-nums">{s?.prefetch.queued ?? 0}</p>
              <p className="text-[11px] text-fg-muted">{t("Queued")}</p>
            </div>
          </div>
          <dl className="space-y-3 text-xs">
            {[
              [t("Detail-page openings"), warming && p.detailWarmup !== "off"],
              [t("Next episode"), warming && p.prefetchNextEpisode],
              [t("Resume warming"), warming && p.keepInProgressWarm],
            ].map(([label, on]) => (
              <div key={String(label)} className="flex justify-between gap-3">
                <dt className="text-fg-muted">{label}</dt>
                <dd className={on ? "font-medium text-primary" : "text-fg-subtle"}>
                  {on ? t("On") : t("Off")}
                </dd>
              </div>
            ))}
          </dl>
          <div className="flex flex-wrap gap-x-4 gap-y-1 border-t border-border pt-3 text-[11px] text-fg-muted">
            <span>{tn(s?.prefetch.completed ?? 0, "{count} completed", "{count} completed")}</span>
            <span>{tn(s?.prefetch.failed ?? 0, "{count} failed", "{count} failed")}</span>
            <span>{tn(s?.prefetch.dropped ?? 0, "{count} dropped", "{count} dropped")}</span>
          </div>
          <Button
            variant="secondary"
            size="sm"
            className="mt-auto w-full"
            disabled={busy || !warming || !p.keepInProgressWarm}
            onClick={onWarm}
          >
            {t("Warm resumes")} <ArrowUpRight size={13} />
          </Button>
        </DashboardPanel>
        <DashboardPanel
          title={t("Policy")}
          icon={<ShieldCheck size={16} />}
          className="lg:col-span-3 cockpit:min-h-0"
          bodyClassName="flex flex-col gap-4 cockpit:overflow-y-auto"
        >
          <div>
            <p className="text-lg font-semibold">{name}</p>
            <p className="mt-1 text-xs text-fg-muted">
              {p.mode === "off"
                ? t("No persistent media")
                : p.mode === "adaptive"
                  ? t("Adaptive retention")
                  : t("Keep recently streamed media")}
            </p>
          </div>
          <dl className="space-y-3 text-xs">
            <div>
              <dt className="text-fg-muted">{t("Expires after")}</dt>
              <dd className="mt-1">
                {p.retentionDays
                  ? tn(p.retentionDays, "{count} inactive day", "{count} inactive days")
                  : t("Only when space is needed")}
              </dd>
            </div>
            <div>
              <dt className="text-fg-muted">{t("Managed preloading")}</dt>
              <dd className="mt-1">
                {!p.preloadEnabled
                  ? t("Disabled")
                  : s?.preload.paused
                    ? t("Paused")
                    : tn(s?.preload.ready ?? 0, "{count} source ready", "{count} sources ready")}
              </dd>
            </div>
          </dl>
          <button
            onClick={onPreloading}
            className="flex items-center gap-1.5 text-left text-xs text-primary hover:underline"
          >
            <Sparkles size={13} /> {t("View preloading")} <ArrowUpRight size={12} />
          </button>
          <Button variant="secondary" size="sm" className="mt-auto w-full" onClick={onProfile}>
            {t("Choose a cache profile")}
          </Button>
        </DashboardPanel>
      </div>
      <p className="shrink-0 text-[10px] text-fg-subtle">
        {t(
          "Live data · refreshes every 5 seconds · hit rate, served and reclaimed counters are since restart.",
        )}
      </p>
    </div>
  );
}
