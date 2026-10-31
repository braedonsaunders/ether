import { getLocale, t, tn } from "../../i18n";
import { Badge, Input, toast } from "@braedonsaunders/appkit-ui";
import { useMemo, useState, type ReactNode } from "react";
import { Meter } from "../../components/charts";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { NumberInput } from "../../components/fields";
import { Section } from "../../components/layout";
import { SaveBar, useDraft } from "../../components/SaveBar";
import { apiFetch } from "../../lib/api";
import { fixed, formatNumber, formatPercent } from "../../lib/format";
import { useAsync } from "../../lib/hooks";

type Provider = {
  id: string;
  name: string;
  host: string;
  state: string;
  backfill: boolean;
  avgLatency: number;
  bytes: number;
  articles: number;
  misses: number;
  errors: number;
};
type Budget = {
  indexer: string;
  hitsUsed: number;
  hitsRemaining: number;
  grabsUsed: number;
  grabsRemaining: number;
  retryAt?: number;
};
type IndexerUsage = {
  indexer: string;
  searches: number;
  successes: number;
  grabs: number;
  grabSuccess: number;
  budgets: number;
  latencyMs: number;
  ageSeconds: number;
  ageSamples: number;
};
type ProviderMonth = {
  id: string;
  bytes: number;
  articles: number;
  misses: number;
  errors: number;
  backfillSaves: number;
  bodyMisses: number;
  bodyTakedowns: number;
  spillovers?: number;
};
type ProviderDay = {
  id: string;
  day: string;
  bytes: number;
  articles: number;
  misses: number;
  errors: number;
};
type IndexerDay = {
  indexer: string;
  day: string;
  searches: number;
  successes: number;
  grabs: number;
  latencyMs: number;
};
type Kind = "provider" | "indexer";
type Cost = { kind: string; id: string; monthly: number; currency: string };
type Infrastructure = {
  monthPlays: number;
  snapshots: {
    providers?: { data: Provider[] | null; at: number };
    indexers?: { data: Budget[] | null; at: number };
  };
  providerMonth: ProviderMonth[] | null;
  providerDays?: ProviderDay[] | null;
  indexerDays?: IndexerDay[] | null;
  indexerMonth: IndexerUsage[] | null;
  costs: Cost[] | null;
  month: string;
  notes: string[] | null;
  failovers?: number;
  failedPlays?: number;
};

type Price = { monthly: number; currency: string };
type Prices = Record<string, Price>;

const GB = 1073741824;
const priceKey = (kind: Kind, id: string) => `${kind}:${id}`;
const pct = (n: number, d: number) => (d ? formatPercent(n / d, 0) : "—");

function DayBars({ days }: { days: { day: string; value: number }[] }) {
  const recent = days.slice(-14);
  const max = Math.max(1, ...recent.map((d) => d.value));
  if (!recent.length) return null;
  return (
    <div className="flex h-8 items-end gap-0.5" aria-label={t("Downloaded each day")}>
      {recent.map((d) => (
        <div
          key={d.day}
          title={d.day}
          className="w-1.5 rounded-sm bg-primary/70"
          style={{ height: `${Math.max(12, (d.value / max) * 100)}%` }}
        />
      ))}
    </div>
  );
}

function Stat({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="min-w-0">
      <dt className="text-fg-subtle">{label}</dt>
      <dd className="truncate tabular-nums text-fg">{children}</dd>
    </div>
  );
}

/** Monthly plan price for one service, edited in place. */
function PriceEditor({
  label,
  value,
  onChange,
  detail,
}: {
  label: string;
  value: Price;
  onChange: (next: Price) => void;
  detail?: ReactNode;
}) {
  return (
    <div className="w-full shrink-0 space-y-1 sm:w-auto">
      <div className="flex items-center gap-1.5">
        <NumberInput
          aria-label={t("Monthly price for {name}", { name: label })}
          className="w-24"
          min={0}
          step={0.01}
          value={value.monthly}
          onChange={(monthly) => onChange({ ...value, monthly })}
        />
        <Input
          aria-label={t("Currency for {name}", { name: label })}
          className="w-16 uppercase"
          maxLength={3}
          value={value.currency}
          onChange={(e) => onChange({ ...value, currency: e.target.value.toUpperCase() })}
        />
        <span className="text-xs text-fg-muted">{t("/ month")}</span>
      </div>
      {detail ? <p className="text-xs text-fg-muted sm:text-right">{detail}</p> : null}
    </div>
  );
}

function Remaining({
  used,
  remaining,
  kind,
}: {
  used: number;
  remaining: number;
  kind: "hits" | "grabs";
}) {
  if (remaining < 0)
    return (
      <p className="text-xs text-fg-muted">
        {kind === "hits"
          ? tn(
              used,
              "{count} API hit today · automatic limits",
              "{count} API hits today · automatic limits",
            )
          : tn(
              used,
              "{count} grab today · automatic limits",
              "{count} grabs today · automatic limits",
            )}
      </p>
    );
  const limit = used + remaining;
  return (
    <Meter
      value={limit ? used / limit : 0}
      tone={limit && used / limit > 0.9 ? "warning" : "primary"}
      label={kind === "hits" ? t("API hits today") : t("Grabs today")}
      valueLabel={t("{used} of {limit}", {
        used: formatNumber(used, 0),
        limit: formatNumber(limit, 0),
      })}
    />
  );
}

/**
 * What Usenet providers and indexers did this month, today's indexer budgets,
 * and the plan prices that turn usage into cost estimates.
 */
export function SourceUsage() {
  const infra = useAsync(() => apiFetch<Infrastructure>("/stats/infrastructure"), []);
  const data = infra.data;

  const saved = useMemo<Prices | null>(() => {
    if (!data) return null;
    const out: Prices = {};
    for (const p of data.snapshots.providers?.data ?? [])
      out[priceKey("provider", p.id || p.host)] = { monthly: 0, currency: "USD" };
    for (const ix of data.snapshots.indexers?.data ?? [])
      out[priceKey("indexer", ix.indexer)] = { monthly: 0, currency: "USD" };
    for (const c of data.costs ?? [])
      if (c.kind === "provider" || c.kind === "indexer")
        out[priceKey(c.kind, c.id)] = { monthly: c.monthly, currency: c.currency };
    return out;
  }, [data]);
  const { draft, setDraft, dirty, reset } = useDraft(saved);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<unknown>(null);

  if (infra.error && !data)
    return <ErrorPanel error={infra.error} onRetry={() => void infra.reload()} />;
  if (!data || !draft || !saved) return <LoadingRows rows={4} />;

  const price = (kind: Kind, id: string) =>
    draft[priceKey(kind, id)] ?? { monthly: 0, currency: "USD" };
  const setPrice = (kind: Kind, id: string, next: Price) =>
    setDraft({ ...draft, [priceKey(kind, id)]: next });

  const save = async () => {
    setSaving(true);
    setError(null);
    try {
      const changed = Object.entries(draft).filter(
        ([k, v]) => JSON.stringify(v) !== JSON.stringify(saved[k]),
      );
      for (const [k, v] of changed) {
        const [kind, ...rest] = k.split(":");
        await apiFetch("/stats/costs", {
          method: "PUT",
          body: JSON.stringify({ kind, id: rest.join(":"), ...v }),
        });
      }
      toast.success(changed.length === 1 ? t("Plan price saved") : t("Plan prices saved"));
      await infra.reload();
    } catch (e) {
      setError(e);
    } finally {
      setSaving(false);
    }
  };

  const providers = data.snapshots.providers?.data ?? [];
  const indexers = data.snapshots.indexers?.data ?? [];
  const savedOnly = (kind: Kind, live: string[]) =>
    (data.costs ?? []).filter((c) => c.kind === kind && !live.includes(c.id)).map((c) => c.id);
  const orphanProviders = savedOnly(
    "provider",
    providers.map((p) => p.id || p.host),
  );
  const orphanIndexers = savedOnly(
    "indexer",
    indexers.map((ix) => ix.indexer),
  );

  const totals = new Map<string, number>();
  for (const v of Object.values(draft))
    if (v.monthly > 0) totals.set(v.currency, (totals.get(v.currency) ?? 0) + v.monthly);

  return (
    <>
      <p className="text-sm text-fg-muted">
        {t("How each source did this month. Plan prices are the last section.")}
      </p>
      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        {[
          [t("Plays this month"), formatNumber(data.monthPlays, 0)],
          [t("Failovers"), formatNumber(data.failovers ?? 0, 0)],
          [t("Failed plays"), formatNumber(data.failedPlays ?? 0, 0)],
          [
            t("Article success"),
            pct(
              (data.providerMonth ?? []).reduce((n, p) => n + p.articles, 0),
              (data.providerMonth ?? []).reduce(
                (n, p) => n + p.articles + p.misses + p.errors,
                0,
              ),
            ),
          ],
        ].map(([label, value]) => (
          <div key={label} className="rounded-xl border border-border bg-surface px-5 py-4">
            <p className="text-xs text-fg-muted">{label}</p>
            <p className="mt-2 text-2xl font-semibold tabular-nums">{value}</p>
          </div>
        ))}
      </div>
      <Section
        title={t("Monthly costs")}
        description={t(
          "Enter a monthly equivalent for annual or block plans. Estimates use usage observed this UTC month; currencies are kept separate.",
        )}
      >
        {totals.size ? (
          <dl className="grid gap-4 text-sm sm:grid-cols-3">
            {[...totals].map(([currency, total]) => (
              <Stat key={currency} label={t("{currency} per month", { currency })}>
                <span className="text-xl font-semibold">{fixed(total, 2)}</span>
                <span className="block text-xs text-fg-muted">
                  {data.monthPlays
                    ? tn(
                        data.monthPlays,
                        "{amount} per play · {count} play this month",
                        "{amount} per play · {count} plays this month",
                        { amount: fixed(total / data.monthPlays, 2) },
                      )
                    : t("No recorded plays this month")}
                </span>
              </Stat>
            ))}
          </dl>
        ) : (
          <p className="text-sm text-fg-muted">
            {t("Add your provider and indexer plan prices below to estimate what streaming costs.")}
          </p>
        )}
      </Section>

      <Section
        title={t("Usenet providers")}
        description={
          data.snapshots.providers
            ? t("This month's traffic. Sampled {time}.", {
                time: new Date(data.snapshots.providers.at * 1000).toLocaleString(getLocale()),
              })
            : t("No provider sample yet.")
        }
        bodyClassName="divide-y divide-border"
      >
        {providers.map((p) => {
          const id = p.id || p.host;
          const label = p.name || p.id || p.host;
          const usage = (data.providerMonth ?? []).find((v) => v.id === id);
          const cost = price("provider", id);
          const perGB =
            usage && usage.bytes > 0 && cost.monthly > 0
              ? t("{amount} {currency}/GB observed", {
                  amount: fixed(cost.monthly / (usage.bytes / GB), 2),
                  currency: cost.currency,
                })
              : undefined;
          return (
            <div key={id} className="flex flex-wrap items-start justify-between gap-4 px-5 py-4">
              <div className="min-w-0 flex-1 space-y-2">
                <div className="flex flex-wrap items-center gap-2">
                  <p className="font-medium text-fg">{label}</p>
                  <Badge variant={p.state === "ok" ? "success" : "warning"}>
                    {p.state === "ok" ? t("Healthy") : p.state || t("Unknown")}
                  </Badge>
                  {p.backfill ? <Badge variant="secondary">{t("Backfill")}</Badge> : null}
                </div>
                {usage ? (
                  <>
                    <dl className="grid grid-cols-2 gap-x-6 gap-y-1 text-xs sm:grid-cols-4">
                      <Stat label={t("This month")}>{fixed(usage.bytes / GB, 2)} GB</Stat>
                      <Stat label={t("Articles")}>{formatNumber(usage.articles, 0)}</Stat>
                      <Stat label={t("Body availability")}>
                        {pct(usage.articles, usage.articles + usage.bodyMisses)}
                      </Stat>
                      <Stat label={t("Latency")}>{fixed(p.avgLatency / 1000000, 0)} ms</Stat>
                    </dl>
                    <p className="text-xs text-fg-muted">
                      {[
                        tn(usage.misses, "{count} miss", "{count} misses"),
                        tn(usage.errors, "{count} error", "{count} errors"),
                        tn(
                          usage.bodyTakedowns,
                          "{count} takedown (451)",
                          "{count} takedowns (451)",
                        ),
                        tn(usage.backfillSaves, "{count} backfill save", "{count} backfill saves"),
                        usage.spillovers
                          ? tn(
                              usage.spillovers,
                              "{count} request taken over from a busy provider",
                              "{count} requests taken over from busy providers",
                            )
                          : "",
                      ]
                        .filter(Boolean)
                        .join(" · ")}
                    </p>
                    <DayBars
                      days={(data.providerDays ?? [])
                        .filter((d) => d.id === id)
                        .map((d) => ({ day: d.day, value: d.bytes }))}
                    />
                    <p className="text-xs text-fg-muted">
                      {t("Article success")}{" "}
                      {pct(usage.articles, usage.articles + usage.misses + usage.errors)}
                    </p>
                  </>
                ) : (
                  <p className="text-xs text-fg-muted">
                    {t("Latency {ms} ms · no usage recorded this month", {
                      ms: fixed(p.avgLatency / 1000000, 0),
                    })}
                  </p>
                )}
              </div>
              <PriceEditor
                label={label}
                value={cost}
                onChange={(v) => setPrice("provider", id, v)}
                detail={perGB}
              />
            </div>
          );
        })}
        {orphanProviders.map((id) => (
          <div key={id} className="flex flex-wrap items-center justify-between gap-4 px-5 py-4">
            <div>
              <p className="font-medium text-fg">{id}</p>
              <p className="text-xs text-fg-muted">{t("Saved plan · no current sample")}</p>
            </div>
            <PriceEditor
              label={id}
              value={price("provider", id)}
              onChange={(v) => setPrice("provider", id, v)}
            />
          </div>
        ))}
        {!providers.length && !orphanProviders.length ? (
          <p className="px-5 py-4 text-sm text-fg-muted">{t("No enabled providers.")}</p>
        ) : null}
      </Section>

      <Section
        title={t("Indexers")}
        description={t(
          "Observed usage today and search results this month. Service limits are handled automatically.",
        )}
        bodyClassName="divide-y divide-border"
      >
        {indexers.map((ix) => {
          const v = (data.indexerMonth ?? []).find((u) => u.indexer === ix.indexer);
          return (
            <div
              key={ix.indexer}
              className="flex flex-wrap items-start justify-between gap-4 px-5 py-4"
            >
              <div className="min-w-0 flex-1 space-y-2">
                <p className="font-medium text-fg">{ix.indexer}</p>
                {ix.retryAt && ix.retryAt > Date.now() / 1000 ? (
                  <p className="text-xs text-fg-muted">
                    {t("Service limit reached · retries automatically after {time}", {
                      time: new Date(ix.retryAt * 1000).toLocaleTimeString(getLocale()),
                    })}
                  </p>
                ) : null}
                <div className="grid max-w-xl gap-3 sm:grid-cols-2">
                  <Remaining used={ix.hitsUsed} remaining={ix.hitsRemaining} kind="hits" />
                  <Remaining used={ix.grabsUsed} remaining={ix.grabsRemaining} kind="grabs" />
                </div>
                {v ? (
                  <dl className="grid grid-cols-2 gap-x-6 gap-y-1 text-xs sm:grid-cols-4">
                    <Stat label={t("Searches this month")}>{formatNumber(v.searches, 0)}</Stat>
                    <Stat label={t("Search success")}>{pct(v.successes, v.searches)}</Stat>
                    <Stat label={t("Grab success")}>
                      {t("{percent} of {total}", {
                        percent: pct(v.grabSuccess, v.grabs),
                        total: formatNumber(v.grabs, 0),
                      })}
                    </Stat>
                    <Stat label={t("Search speed")}>
                      {v.searches
                        ? t("{ms} ms", { ms: fixed(v.latencyMs / v.searches, 0) })
                        : "—"}
                    </Stat>
                    <Stat label={t("Average release age")}>
                      {v.ageSamples
                        ? t("{days} days", { days: fixed(v.ageSeconds / v.ageSamples / 86400, 1) })
                        : "—"}
                    </Stat>
                  </dl>
                ) : (
                  <p className="text-xs text-fg-muted">{t("No searches recorded this month.")}</p>
                )}
              </div>
              <PriceEditor
                label={ix.indexer}
                value={price("indexer", ix.indexer)}
                onChange={(p) => setPrice("indexer", ix.indexer, p)}
              />
            </div>
          );
        })}
        {orphanIndexers.map((id) => (
          <div key={id} className="flex flex-wrap items-center justify-between gap-4 px-5 py-4">
            <div>
              <p className="font-medium text-fg">{id}</p>
              <p className="text-xs text-fg-muted">{t("Saved plan · no current sample")}</p>
            </div>
            <PriceEditor
              label={id}
              value={price("indexer", id)}
              onChange={(p) => setPrice("indexer", id, p)}
            />
          </div>
        ))}
        {!indexers.length && !orphanIndexers.length ? (
          <p className="px-5 py-4 text-sm text-fg-muted">{t("No configured indexers.")}</p>
        ) : null}
      </Section>

      <SaveBar
        dirty={dirty}
        saving={saving}
        error={error}
        saveLabel={t("Save prices")}
        onSave={save}
        onDiscard={() => {
          reset();
          setError(null);
        }}
      />
    </>
  );
}
