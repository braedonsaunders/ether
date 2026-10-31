import {
  Badge,
  Button,
  EmptyState,
  PagedTable,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@braedonsaunders/appkit-ui";
import { CheckCircle2, Pin as PinIcon, RefreshCw, Repeat, Route, XCircle } from "lucide-react";
import { useMemo, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { Drawer } from "../../components/Drawer";
import { t, tn, tx } from "../../i18n";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import {
  formatBytes,
  formatDateTime,
  formatNanos,
  isPlainObject,
  relativeTime,
} from "../../lib/format";
import { useAsync } from "../../lib/hooks";
import { releaseConsole, type HistoryRecord } from "../../lib/releases";
import type { CandidateTrace, ResolverTrace } from "../../lib/types";
import { pagedTableLabels } from "../../i18n/appkit-labels";
import { useLocale } from "../../i18n/react";
import { searchLink } from "./Pinned";

/** A max-height ceiling from loose JSON: a positive number, else 0 ("Any"). */
function ceiling(v: unknown): number {
  const n = Number(v ?? 0);
  return Number.isFinite(n) && n > 0 ? n : 0;
}

/** A configured ceiling with presence: absent stays undefined (unknown, as
 * on legacy traces that never recorded it); an explicit 0 is "Any". */
function configuredCeiling(v: unknown): number | undefined {
  if (v === undefined || v === null) return undefined;
  const n = Number(v);
  return Number.isFinite(n) && n >= 0 ? n : undefined;
}

function normalizeTrace(raw: Record<string, unknown>, i: number): ResolverTrace {
  // Ceilings may ride on the record itself, on a history entry's nested
  // trace, or in the trace's limits (profileConfiguredMaxHeight /
  // profileMaxHeight is the effective value there); the record's own
  // values win, then the nested trace's.
  const nested = isPlainObject(raw.trace) ? raw.trace : {};
  const limits = isPlainObject(raw.limits) ? raw.limits : {};
  const nestedLimits = isPlainObject(nested.limits) ? nested.limits : {};
  return {
    titleKey: String(raw.titleKey ?? raw.key ?? i),
    titleName: String(raw.titleName ?? raw.title ?? raw.name ?? t("Trace {n}", { n: i + 1 })),
    profile: String(raw.profile ?? ""),
    profileMaxHeight: configuredCeiling(
      raw.profileMaxHeight ??
        nested.profileMaxHeight ??
        limits.profileConfiguredMaxHeight ??
        nestedLimits.profileConfiguredMaxHeight,
    ),
    clientMaxHeight: ceiling(
      raw.clientMaxHeight ??
        nested.clientMaxHeight ??
        limits.clientMaxHeight ??
        nestedLimits.clientMaxHeight,
    ),
    effectiveMaxHeight: ceiling(
      raw.effectiveMaxHeight ??
        nested.effectiveMaxHeight ??
        limits.profileMaxHeight ??
        nestedLimits.profileMaxHeight,
    ),
    at: String(raw.at ?? ""),
    candidates: (Array.isArray(raw.candidates)
      ? raw.candidates
      : Array.isArray(raw.releases)
        ? raw.releases
        : []
    )
      .filter(isPlainObject)
      .map((c) => ({
        releaseId: String(c.releaseId ?? c.id ?? ""),
        name: String(c.name ?? ""),
        indexer: String(c.indexer ?? ""),
        score: Number(c.score ?? 0),
        rejected: Boolean(c.rejected),
        reason: typeof c.reason === "string" ? c.reason : undefined,
        healthChecked: Boolean(c.healthChecked),
        healthy: Boolean(c.healthy),
        fetchDuration: Number(c.fetchDuration ?? 0),
        healthDuration: Number(c.healthDuration ?? 0),
        size: Number(c.size ?? 0) || undefined,
        pinned: Boolean(c.pinned),
        customFormats: Array.isArray(c.customFormats)
          ? c.customFormats.filter(isPlainObject).map((f) => ({
              id: String(f.id ?? ""),
              name: String(f.name ?? f.id ?? ""),
              score: Number(f.score ?? 0),
            }))
          : undefined,
        compatibilityReason:
          typeof c.compatibilityReason === "string" ? c.compatibilityReason : undefined,
        route: typeof c.route === "string" ? c.route : undefined,
      })),
    winner: typeof raw.winner === "string" ? raw.winner : undefined,
    err:
      typeof raw.err === "string" ? raw.err : typeof raw.error === "string" ? raw.error : undefined,
    searchDuration: Number(raw.searchDuration ?? 0),
    failover: Boolean(raw.failover),
    pinned: typeof raw.pinned === "string" ? raw.pinned : undefined,
    pinStatus: typeof raw.pinStatus === "string" ? raw.pinStatus : undefined,
    omitted: Number(raw.omitted ?? 0) || undefined,
    routing: typeof raw.routing === "string" ? raw.routing : undefined,
    chooser: chooserOf(raw.chooser ?? nested.chooser),
    chosenVersion:
      typeof raw.chosenVersion === "string"
        ? raw.chosenVersion
        : typeof nested.chosenVersion === "string"
          ? nested.chosenVersion
          : undefined,
    discovery: Array.isArray(raw.discovery)
      ? raw.discovery.filter(isPlainObject).map((d) => ({
          path: String(d.path ?? "unknown"),
          count: Number(d.count ?? 0),
          rescue: Boolean(d.rescue),
        }))
      : undefined,
    limits: isPlainObject(raw.limits)
      ? {
          profileMinHeight: Number(raw.limits.profileMinHeight ?? 0),
          profileMaxHeight: Number(raw.limits.profileMaxHeight ?? 0),
          profileConfiguredMaxHeight: configuredCeiling(raw.limits.profileConfiguredMaxHeight),
          clientMaxHeight: Number(raw.limits.clientMaxHeight ?? 0),
          clientMaxBitrateKbps: Number(raw.limits.clientMaxBitrateKbps ?? 0),
        }
      : undefined,
  };
}

/** Accept the resolver's typed traces, and wrap anything else so it still renders. */
export function normalizeTraces(data: unknown): ResolverTrace[] {
  const list = Array.isArray(data)
    ? data
    : isPlainObject(data) && Array.isArray(data.traces)
      ? data.traces
      : [];
  return list.filter(isPlainObject).map(normalizeTrace);
}

function chooserOf(v: unknown): "auto" | "viewer" | undefined {
  return v === "auto" || v === "viewer" ? v : undefined;
}

/** Who chose the release. Absent when the decision predates the field. */
export function choiceLabel(chooser?: string, version?: string): string | null {
  if (chooser === "viewer") {
    return version
      ? t("Viewer picked version {version}", { version })
      : t("Viewer picked a version");
  }
  if (chooser === "auto") return t("Auto");
  return null;
}

function outcome(h: { error?: string; err?: string; releaseId?: string; winner?: string }) {
  if (h.error || h.err)
    return (
      <Badge variant="destructive">
        <XCircle size={11} /> {t("Failed")}
      </Badge>
    );
  if (h.releaseId || h.winner)
    return (
      <Badge variant="success">
        <CheckCircle2 size={11} /> {t("Picked")}
      </Badge>
    );
  return <Badge variant="secondary">{t("No pick")}</Badge>;
}

function flags(h: { failover?: boolean; pinned?: boolean }) {
  return (
    <>
      {h.failover ? (
        <Badge variant="warning">
          <Repeat size={11} /> {t("Failover")}
        </Badge>
      ) : null}
      {h.pinned ? (
        <Badge variant="info">
          <PinIcon size={11} /> {t("Pinned")}
        </Badge>
      ) : null}
    </>
  );
}

/** Every release decision Ether has made, newest first; one opens in a drawer. */
export function HistoryTab() {
  const [params, setParams] = useSearchParams();
  const key = params.get("key") ?? "";
  const titleName = params.get("title") ?? "";
  const state = useAsync((signal) => releaseConsole.history({ key, limit: 500 }, signal), [key]);
  const [open, setOpen] = useState<HistoryRecord | null>(null);
  const detail = useAsync(
    () => (open ? releaseConsole.historyEntry(open.id) : Promise.resolve(null)),
    [open?.id],
  );
  const { locale } = useLocale();
  const trace = useMemo(() => {
    if (!detail.data || detail.data.id !== open?.id || !isPlainObject(detail.data.trace))
      return null;
    const raw = detail.data.trace as unknown as Record<string, unknown>;
    // The entry record also carries the ceilings (the listing surfaces
    // them); fall back to them when the stored trace predates them.
    return normalizeTrace(
      {
        ...raw,
        profileMaxHeight: raw.profileMaxHeight ?? detail.data.profileMaxHeight,
        clientMaxHeight: raw.clientMaxHeight ?? detail.data.clientMaxHeight,
        effectiveMaxHeight: raw.effectiveMaxHeight ?? detail.data.effectiveMaxHeight,
      },
      0,
    );
  }, [detail.data, open?.id, locale]);
  const rows = state.data?.items ?? [];

  return (
    <>
      <div className="flex flex-wrap items-center justify-between gap-3">
        <p className="max-w-2xl text-sm text-fg-muted">
          {key ? (
            <>
              {tx("Decisions for {title}.", {
                title: <span className="font-medium text-fg">{titleName || key}</span>,
              })}{" "}
              <button
                type="button"
                className="text-primary underline"
                onClick={() => setParams({})}
              >
                {t("Show every title")}
              </button>
            </>
          ) : (
            t(
              "Why each release was picked or rejected: profile match, size, custom formats, pins, health checks and failovers.",
            )
          )}
        </p>
        <Button variant="outline" size="sm" onClick={() => void state.reload()}>
          <RefreshCw size={14} /> {t("Refresh")}
        </Button>
      </div>
      {state.error ? (
        <ErrorPanel error={state.error} onRetry={() => void state.reload()} />
      ) : !state.data ? (
        <LoadingRows rows={5} />
      ) : (
        <>
          {state.data.total > rows.length ? (
            <p className="text-xs text-fg-muted">
              {tn(
                state.data.total,
                "Showing the newest {shown} of {count} decision.",
                "Showing the newest {shown} of {count} decisions.",
                { shown: rows.length },
              )}
            </p>
          ) : null}
          <PagedTable<HistoryRecord>
            rows={rows}
            rowKey={(h) => String(h.id)}
            searchable
            pageSize={25}
            onRowClick={setOpen}
            labels={pagedTableLabels({
              searchLabel: t("Search release history"),
              searchPlaceholder: t("Filter by title, profile or release…"),
            })}
            empty={
              <EmptyState
                icon={<Route />}
                title={t("No decisions yet")}
                description={t("Play a title. The decision appears here.")}
              />
            }
            columns={[
              {
                key: "title",
                header: t("Title"),
                search: (h) => `${h.titleName} ${h.profile ?? ""} ${h.releaseName ?? ""}`,
                sortValue: (h) => h.titleName,
                cell: (h) => (
                  <span>
                    <span className="block font-medium text-fg">{h.titleName}</span>
                    {h.error ? (
                      <span className="block truncate text-xs text-danger">{h.error}</span>
                    ) : (
                      <span className="block max-w-md truncate font-mono text-xs text-fg-muted">
                        {h.releaseName}
                      </span>
                    )}
                  </span>
                ),
              },
              {
                key: "outcome",
                header: t("Outcome"),
                cell: (h) => {
                  const choice = choiceLabel(h.chooser, h.chosenVersion);
                  return (
                    <span className="flex flex-wrap gap-1">
                      {outcome(h)}
                      {choice ? <Badge variant="secondary">{choice}</Badge> : null}
                      {flags(h)}
                    </span>
                  );
                },
              },
              {
                key: "profile",
                header: t("Profile"),
                sortValue: (h) => h.profile ?? "",
                cell: (h) => (h.profile ? <Badge variant="secondary">{h.profile}</Badge> : "—"),
              },
              {
                key: "candidates",
                header: t("Releases"),
                align: "right",
                sortValue: (h) => h.candidates,
                cell: (h) => <span className="tabular-nums">{h.candidates}</span>,
              },
              {
                key: "at",
                header: t("When"),
                sortValue: (h) => h.at,
                cell: (h) => (
                  <span className="text-fg-muted" title={formatDateTime(h.at)}>
                    {relativeTime(h.at)}
                  </span>
                ),
              },
            ]}
            defaultSort={{ key: "at", dir: "desc" }}
          />
        </>
      )}
      <Drawer
        open={open !== null}
        onClose={() => setOpen(null)}
        size="xl"
        title={open?.titleName}
        description={
          open
            ? [
                open.profile ? t("Profile {profile}", { profile: open.profile }) : "",
                formatDateTime(open.at),
                trace
                  ? t("search took {duration}", { duration: formatNanos(trace.searchDuration) })
                  : "",
              ]
                .filter(Boolean)
                .join(" · ")
            : undefined
        }
      >
        {open ? (
          detail.error ? (
            <ErrorPanel error={detail.error} onRetry={() => void detail.reload()} />
          ) : detail.data?.id !== open.id ? (
            <LoadingRows rows={4} />
          ) : trace ? (
            <TraceDetail trace={trace} titleKey={open.titleKey} />
          ) : (
            <EmptyState
              title={t("No details stored")}
              description={t("This decision has no trace.")}
            />
          )
        ) : null}
      </Drawer>
    </>
  );
}

function verdict(c: CandidateTrace, trace: ResolverTrace) {
  if (c.releaseId && c.releaseId === trace.winner)
    return (
      <Badge variant="success">
        <CheckCircle2 size={11} /> {t("Picked")}
      </Badge>
    );
  if (c.rejected) return <Badge variant="destructive">{t("Rejected")}</Badge>;
  return <Badge variant="secondary">{t("Fallback")}</Badge>;
}

function profileResolution(min: number, max: number) {
  if (min && max) return `${min}p–${max}p`;
  if (min) return t("At least {height}p", { height: min });
  if (max) return t("Up to {height}p", { height: max });
  return t("Any resolution");
}

function discoveryLabel(path: string) {
  switch (path) {
    case "feed":
      return t("Feed results");
    case "cache":
      return t("Cached search");
    case "live-complete":
      return t("Live search completed");
    case "live-early":
      return t("Live search ended early");
    default:
      return t("Unknown discovery");
  }
}

/**
 * "Profile ≤2160 · Client ≤1080 · Effective ≤1080" for a decision, or
 * null when the configured ceiling was never recorded (legacy traces:
 * unknown is not "Any"). A recorded 0 is explicitly unlimited:
 * "Profile Any", the Quality page's word for no ceiling.
 */
export function resolutionCeilings(trace: ResolverTrace): string | null {
  if (trace.profileMaxHeight === undefined) return null;
  const parts: Array<[string, number | undefined]> = [
    [t("Profile"), trace.profileMaxHeight],
    [t("Client"), trace.clientMaxHeight],
    [t("Effective"), trace.effectiveMaxHeight],
  ];
  return parts.map(([label, h]) => (h ? `${label} ≤${h}` : `${label} ${t("Any")}`)).join(" · ");
}

/** Every candidate of one decision and why it won or lost. */
export function TraceDetail({ trace, titleKey }: { trace: ResolverTrace; titleKey?: string }) {
  const ceilings = resolutionCeilings(trace);
  const cands = [...(trace.candidates ?? [])].sort(
    (a, b) =>
      Number(b.releaseId === trace.winner) - Number(a.releaseId === trace.winner) ||
      Number(a.rejected) - Number(b.rejected) ||
      b.score - a.score,
  );
  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center gap-2">
        {outcome(trace)}
        {choiceLabel(trace.chooser, trace.chosenVersion) ? (
          <Badge variant="secondary">{choiceLabel(trace.chooser, trace.chosenVersion)}</Badge>
        ) : null}
        {flags({ failover: trace.failover, pinned: trace.pinStatus === "used" })}
        {trace.err ? <span className="text-sm text-danger">{trace.err}</span> : null}
      </div>
      {ceilings ? <p className="text-sm text-fg-muted">{ceilings}</p> : null}
      {trace.routing ? (
        <p className="text-sm text-fg-muted">
          {t("Source routing: {routing}", { routing: trace.routing })}
        </p>
      ) : null}
      <div className="text-sm text-fg-muted">
        <p>
          {t("Discovery:")}{" "}
          {trace.discovery?.length
            ? trace.discovery
                .map((d) => {
                  const releases = tn(d.count, "{count} release", "{count} releases");
                  const label = discoveryLabel(d.path);
                  return d.rescue
                    ? t("Rescue: {source} ({releases})", { source: label, releases })
                    : t("{source} ({releases})", { source: label, releases });
                })
                .join(" · ")
            : t("unknown")}
        </p>
        <p>
          {t("Effective playback limits:")}{" "}
          {trace.limits
            ? t("Profile: {profile} · Height: {height} · Bitrate: {bitrate}", {
                profile: profileResolution(
                  trace.limits.profileMinHeight,
                  trace.limits.profileMaxHeight,
                ),
                height: trace.limits.clientMaxHeight
                  ? `${trace.limits.clientMaxHeight}p`
                  : t("unlimited"),
                bitrate: trace.limits.clientMaxBitrateKbps
                  ? t("{rate} kbps", { rate: trace.limits.clientMaxBitrateKbps })
                  : t("unlimited"),
              })
            : t("unknown")}
        </p>
      </div>
      {trace.pinStatus && trace.pinStatus !== "used" ? (
        <p className="text-sm text-fg-muted">
          <PinIcon size={13} className="mr-1 inline" aria-hidden />
          {t("Owner pin: {status}", { status: trace.pinStatus })}
        </p>
      ) : null}
      {titleKey ? (
        <p className="text-sm">
          <Link className="text-primary underline" to={searchLink(titleKey, trace.titleName)}>
            {t("Search releases for this title")}
          </Link>
        </p>
      ) : null}
      {cands.length === 0 ? (
        <EmptyState
          title={t("No releases found")}
          description={t("The indexers returned nothing usable for this title.")}
        />
      ) : (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>{t("Release")}</TableHead>
              <TableHead className="text-right">{t("Score")}</TableHead>
              <TableHead>{t("Health")}</TableHead>
              <TableHead>{t("Verdict")}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {cands.map((c: CandidateTrace, i) => (
              <TableRow key={`${c.releaseId || c.name}-${i}`}>
                <TableCell>
                  <span className="block break-all font-mono text-xs text-fg">{c.name}</span>
                  <span className="text-xs text-fg-muted">
                    {c.indexer}
                    {c.size ? ` · ${formatBytes(c.size)}` : ""}
                    {c.fetchDuration
                      ? ` · ${t("fetched in {duration}", { duration: formatNanos(c.fetchDuration) })}`
                      : ""}
                  </span>
                  {c.pinned || c.customFormats?.length ? (
                    <span className="mt-1 flex flex-wrap gap-1">
                      {c.pinned ? <Badge variant="info">{t("Pinned")}</Badge> : null}
                      {(c.customFormats ?? []).map((f) => (
                        <Badge key={f.id} variant="outline">
                          {f.name} {f.score > 0 ? `+${f.score}` : f.score}
                        </Badge>
                      ))}
                    </span>
                  ) : null}
                </TableCell>
                <TableCell className="text-right tabular-nums">
                  {c.rejected && !c.score ? "—" : c.score.toFixed(0)}
                </TableCell>
                <TableCell>
                  {c.healthChecked ? (
                    c.healthy ? (
                      <Badge variant="success">{t("Healthy")}</Badge>
                    ) : (
                      <Badge variant="destructive">{t("Failed")}</Badge>
                    )
                  ) : (
                    <span className="text-xs text-fg-subtle">{t("Not checked")}</span>
                  )}
                  {c.healthDuration ? (
                    <span className="block text-[11px] text-fg-subtle">
                      {formatNanos(c.healthDuration)}
                    </span>
                  ) : null}
                </TableCell>
                <TableCell>
                  {verdict(c, trace)}
                  {c.reason ? (
                    <span className="mt-1 block text-xs text-fg-muted">{c.reason}</span>
                  ) : null}
                  {c.compatibilityReason ? (
                    <span className="mt-1 block text-[11px] text-fg-subtle">
                      {c.compatibilityReason}
                    </span>
                  ) : null}
                  {c.route ? (
                    <span className="mt-1 block text-[11px] text-fg-subtle">{c.route}</span>
                  ) : null}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
      {trace.omitted ? (
        <p className="text-xs text-fg-muted">
          {tn(
            trace.omitted,
            "{count} more rejected release not stored.",
            "{count} more rejected releases not stored.",
          )}
        </p>
      ) : null}
    </div>
  );
}
