import { Badge, Button, EmptyState, toast } from "@braedonsaunders/appkit-ui";
import { History, RotateCcw } from "lucide-react";
import { useState } from "react";
import { Link } from "react-router-dom";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { Section } from "../../components/layout";
import { SaveBar, useDraft } from "../../components/SaveBar";
import { viewerName } from "../../components/pickers";
import { t, tx } from "../../i18n";
import { apiFetch, errorMessage } from "../../lib/api";
import type { Viewer } from "../../lib/contract";
import { useAsync } from "../../lib/hooks";
import {
  normalizeRules,
  sameRules,
  StreamRulesFields,
  type ShowMemory,
  type StreamRules,
} from "./shared";

function choiceText(m: ShowMemory): string {
  if (m.choice.off) return t("Off");
  return [
    m.choice.language || t("Unknown language"),
    m.choice.forced ? t("forced") : "",
    m.choice.sdh ? "SDH" : "",
  ]
    .filter(Boolean)
    .join(" · ");
}

/**
 * One viewer's audio and subtitle choices. They follow Viewers › Defaults
 * until the owner saves something different here.
 */
export function ViewerPlayback({ viewer }: { viewer: Viewer }) {
  const id = viewer.id;
  const name = viewerName(viewer);
  const data = useAsync(
    async (signal) => {
      const [defaults, rules, memory] = await Promise.all([
        apiFetch<StreamRules>("/stream-preferences/0", { signal }),
        apiFetch<StreamRules>(`/stream-preferences/${id}`, { signal }),
        apiFetch<ShowMemory[] | null>(`/stream-preferences/${id}/shows`, { signal }),
      ]);
      return {
        defaults: normalizeRules(defaults),
        rules: normalizeRules(rules),
        memory: memory ?? [],
      };
    },
    [id],
  );
  const { draft, setDraft, dirty, reset } = useDraft<StreamRules>(data.data?.rules);
  const [saving, setSaving] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);

  if (data.error) return <ErrorPanel error={data.error} onRetry={() => void data.reload()} />;
  if (!data.data || !draft) return <LoadingRows rows={4} />;
  const { defaults, rules, memory } = data.data;
  const custom = !sameRules(rules, defaults);

  const save = async () => {
    setSaving(true);
    setError(null);
    try {
      await apiFetch(`/stream-preferences/${id}`, { method: "PUT", body: JSON.stringify(draft) });
      data.setData((d) => ({ ...d!, rules: draft }));
      toast.success(t("Audio and subtitles saved"), {
        description: t("Saved for {name}.", { name }),
      });
    } catch (e) {
      setError(e);
    } finally {
      setSaving(false);
    }
  };

  const restoreDefault = async () => {
    setBusy(true);
    try {
      await apiFetch(`/stream-preferences/${id}`, { method: "DELETE" });
      await data.reload();
      setError(null);
      toast.success(t("Now using the defaults"), {
        description: t("{name} follows Viewers › Defaults.", { name }),
      });
    } catch (e) {
      toast.error(t("Couldn't reset"), { description: errorMessage(e) });
    } finally {
      setBusy(false);
    }
  };

  const forget = async (showId: number) => {
    try {
      await apiFetch(`/stream-preferences/${id}/shows/${showId}`, { method: "DELETE" });
      data.setData((d) => ({ ...d!, memory: d!.memory.filter((m) => m.showId !== showId) }));
    } catch (e) {
      toast.error(t("Couldn't forget the show"), { description: errorMessage(e) });
    }
  };

  return (
    <div className="max-w-3xl space-y-6">
      <div className="flex flex-wrap items-center justify-between gap-3 rounded-xl border border-border bg-surface px-5 py-4">
        <div className="min-w-0">
          <div className="flex flex-wrap items-center gap-2">
            <h2 className="text-sm font-semibold text-fg">{t("Audio and subtitles")}</h2>
            <Badge variant={custom ? "info" : "secondary"}>
              {custom ? t("Custom for this viewer") : t("Uses default")}
            </Badge>
          </div>
          <p className="mt-0.5 text-sm text-fg-muted">
            {custom
              ? t("These choices replace the defaults for this viewer.")
              : tx("Follows {link}. Change anything below to give {name} their own.", {
                  link: (
                    <Link
                      to="/viewers/defaults"
                      className="font-medium text-primary hover:underline"
                    >
                      {t("Viewers › Defaults")}
                    </Link>
                  ),
                  name,
                })}
          </p>
        </div>
        {custom ? (
          <Button variant="outline" size="sm" disabled={busy} onClick={() => void restoreDefault()}>
            <RotateCcw size={14} /> {t("Use default")}
          </Button>
        ) : null}
      </div>

      <StreamRulesFields value={draft} onChange={setDraft} disabled={busy} />

      <Section
        title={t("Show memory")}
        description={t(
          "When this viewer changes tracks in Plex, Ether remembers the choice for later episodes of the same show.",
        )}
      >
        {memory.length === 0 ? (
          <EmptyState
            icon={<History />}
            title={t("No remembered choices")}
            description={t("Show-specific audio and subtitle picks appear here.")}
            className="py-6"
          />
        ) : (
          <ul className="divide-y divide-border">
            {memory.map((m) => (
              <li
                key={`${m.showId}-${m.kind}`}
                className="flex flex-wrap items-center justify-between gap-3 py-2.5 text-sm"
              >
                <span className="min-w-0">
                  <span className="font-medium text-fg">
                    {m.showTitle || t("Unavailable show")}
                  </span>
                  <span className="block text-xs text-fg-muted">
                    {m.kind === "audio"
                      ? t("Audio: {choice}", { choice: choiceText(m) })
                      : t("Subtitles: {choice}", { choice: choiceText(m) })}
                  </span>
                </span>
                <Button size="sm" variant="outline" onClick={() => void forget(m.showId)}>
                  {t("Forget")}
                </Button>
              </li>
            ))}
          </ul>
        )}
      </Section>

      <SaveBar
        dirty={dirty}
        saving={saving}
        error={error}
        onSave={save}
        onDiscard={() => {
          reset();
          setError(null);
        }}
      />
    </div>
  );
}
