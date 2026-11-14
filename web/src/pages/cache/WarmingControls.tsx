import { Button, Switch } from "@braedonsaunders/appkit-ui";
import { Flame } from "lucide-react";
import { useState } from "react";
import { t } from "../../i18n";
import { apiFetch } from "../../lib/api";
import { useAsync } from "../../lib/hooks";
import { ErrorPanel } from "../../components/feedback";

type Warming = { titles: { key: number; title: string }[]; speculative: boolean };
const path = "/cache/warming";

export function WarmingControls({ titleKey }: { titleKey: string }) {
  const state = useAsync(() => apiFetch<Warming>(path), [titleKey]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);
  async function save(body: { key?: number; pinned?: boolean; speculative?: boolean }) {
    setBusy(true);
    setError(null);
    try {
      await apiFetch(path, { method: "POST", body: JSON.stringify(body) });
      await state.reload();
    } catch (e) {
      setError(e);
    } finally {
      setBusy(false);
    }
  }
  const data = state.data;
  const pinned = data?.titles.some((x) => x.key === Number(titleKey)) ?? false;
  const problem =
    state.error || error ? (
      <ErrorPanel error={error ?? state.error} onRetry={() => void state.reload()} />
    ) : null;
  return (
    <>
      <Button
        size="sm"
        variant={pinned ? "default" : "outline"}
        aria-pressed={pinned}
        disabled={!data || busy}
        onClick={() => void save({ key: Number(titleKey), pinned: !pinned })}
      >
        <Flame size={14} fill={pinned ? "currentColor" : "none"} />
        {t("Keep warm")}
      </Button>
      {problem}
    </>
  );
}

/** Changes join the cache policy draft and are persisted by its existing Save bar. */
export function WarmingPolicy({
  keys,
  speculative,
  disabled,
  onRemove,
  onSpeculativeChange,
}: {
  keys: number[];
  speculative: boolean;
  disabled: boolean;
  onRemove: (key: number) => void;
  onSpeculativeChange: (value: boolean) => void;
}) {
  const state = useAsync((signal) => apiFetch<Warming>(path, { signal }), []);
  return (
    <section className="shrink-0 rounded-xl border border-border bg-surface p-4">
      <h3 className="text-sm font-medium">{t("Keep warm")}</h3>
      <p className="mt-1 text-xs text-fg-muted">
        {t("People's next episodes come first, then your pins. Uses your cache budget.")}
      </p>
      <ul className="my-3 max-h-36 space-y-2 overflow-y-auto">
        {keys.map((key) => (
          <li key={key} className="flex items-center justify-between gap-3 text-sm">
            <span>{state.data?.titles.find((title) => title.key === key)?.title ?? key}</span>
            <Button size="sm" disabled={disabled} onClick={() => onRemove(key)}>
              {t("Remove")}
            </Button>
          </li>
        ))}
      </ul>
      {!keys.length && (
        <p className="my-3 text-xs text-fg-muted">{t("Choose Keep warm on a movie or show.")}</p>
      )}
      <div className="flex items-center justify-between gap-4 border-t border-border pt-3">
        <div>
          <label htmlFor="speculative-warming" className="text-sm font-medium">
            {t("Speculative warming")}
          </label>
          <p className="text-xs text-fg-muted">
            {t("Off: only pins, watchlists and what people are watching.")}
          </p>
        </div>
        <Switch
          id="speculative-warming"
          checked={speculative}
          disabled={disabled}
          onChange={(e) => onSpeculativeChange(e.currentTarget.checked)}
        />
      </div>
      {state.error ? <ErrorPanel error={state.error} onRetry={() => void state.reload()} /> : null}
    </section>
  );
}
