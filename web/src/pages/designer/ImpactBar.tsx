import { Button, Spinner } from "@braedonsaunders/appkit-ui";
import { Users } from "lucide-react";
import { useEffect, useState } from "react";
import { api, errorMessage, isAbort } from "../../lib/api";
import type { HomeLayout, LayoutImpactResult } from "../../lib/contract";
import { t, tn, tx } from "../../i18n";
import { useDebouncedValue } from "../../lib/hooks";
import type { ViewContext } from "./ContextBar";

/**
 * What a Global draft does across viewers in this context: every viewer's
 * page with the saved layout against the draft, computed by the server.
 */
export function ImpactBar({
  draft,
  ctx,
  at,
}: {
  draft: HomeLayout;
  saved: HomeLayout;
  ctx: ViewContext;
  at: string;
}) {
  const key = useDebouncedValue(
    JSON.stringify({ draft, device: ctx.device, at, section: ctx.section }),
    500,
  );
  const [result, setResult] = useState<LayoutImpactResult | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [open, setOpen] = useState(false);

  useEffect(() => {
    const r = JSON.parse(key) as {
      draft: HomeLayout;
      device: ViewContext["device"];
      at: string;
      section: ViewContext["section"];
    };
    const ctl = new AbortController();
    setLoading(true);
    api
      .layoutImpact({ layout: r.draft, device: r.device, at: r.at, section: r.section }, ctl.signal)
      .then((res) => {
        setResult(res);
        setError(null);
      })
      .catch((e) => {
        if (!isAbort(e) && !ctl.signal.aborted) setError(errorMessage(e));
      })
      .finally(() => {
        if (!ctl.signal.aborted) setLoading(false);
      });
    return () => ctl.abort();
  }, [key]);

  const s = result?.summary;
  return (
    <div
      className="rounded-xl border border-warning/40 bg-warning-subtle px-3 py-2.5"
      role="status"
      aria-label={t("Impact across viewers")}
    >
      <div className="flex flex-wrap items-center gap-2 text-sm">
        <Users size={15} className="text-warning" />
        {error ? (
          <span className="text-fg-muted">
            {t("Couldn't work out the impact: {error}", { error })}
          </span>
        ) : !s ? (
          <span className="text-fg-muted">{t("Working out who this affects…")}</span>
        ) : (
          <span className="text-fg">
            {tx("Affects {viewers}", {
              viewers: <strong>{tn(s.viewers, "{count} viewer", "{count} viewers")}</strong>,
            })}
            {` · ${tn(s.withOverrides, "{count} has overrides", "{count} have overrides")}`}
            {` · ${tn(s.changed, "{count} Home changes in this context", "{count} Homes change in this context")}`}
          </span>
        )}
        {loading ? <Spinner size={12} label={t("Updating impact")} /> : null}
        {s ? (
          <Button
            variant="ghost"
            size="sm"
            className="ml-auto h-7 px-2 text-xs"
            aria-expanded={open}
            onClick={() => setOpen((o) => !o)}
          >
            {open ? t("Hide details") : t("Details")}
          </Button>
        ) : null}
      </div>
      {open && result ? (
        <ul className="mt-2 space-y-1 text-xs">
          {result.viewers.map((v) => (
            <li key={v.viewerId} className="flex flex-wrap gap-x-2">
              <span className="font-medium text-fg">{v.title}</span>
              {v.overrides ? (
                <span className="text-info">
                  {tn(v.overrides, "{count} override", "{count} overrides")}
                </span>
              ) : null}
              {!v.changed ? (
                <span className="text-fg-muted">{t("no change")}</span>
              ) : (
                <span className="text-fg-muted">
                  {[
                    v.added.length ? `+ ${v.added.join(", ")}` : "",
                    v.removed.length ? `− ${v.removed.join(", ")}` : "",
                    v.moved ? tn(v.moved, "{count} row moved", "{count} rows moved") : "",
                  ]
                    .filter(Boolean)
                    .join(" · ")}
                </span>
              )}
            </li>
          ))}
        </ul>
      ) : null}
    </div>
  );
}
