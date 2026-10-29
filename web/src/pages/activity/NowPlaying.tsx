import { Badge, Button, EmptyState, toast } from "@braedonsaunders/appkit-ui";
import { Radio, Square } from "lucide-react";
import { useState } from "react";
import { Artwork } from "../../components/Artwork";
import { Meter } from "../../components/charts";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { Section } from "../../components/layout";
import { apiFetch, errorMessage } from "../../lib/api";
import { t, tn } from "../../i18n";
import { formatRate, relativeTime } from "../../lib/format";
import type { NowPlaying as Stream } from "../../lib/health";
import type { AsyncState } from "../../lib/hooks";
import { useTitle } from "../../lib/titles";

function decisionLabel(decision: string): string {
  if (decision === "transcode") return t("Transcoding");
  if (decision === "copy") return t("Direct stream");
  return decision ? t("Direct play") : t("Starting");
}

function StreamRow({ s, stopping, onStop }: { s: Stream; stopping: boolean; onStop: () => void }) {
  const title = useTitle(s.key || undefined);
  return (
    <li className="flex flex-wrap items-center gap-4 py-4 first:pt-0 last:pb-0">
      <Artwork
        src={title?.poster}
        alt={s.title}
        title=""
        className="aspect-[2/3] w-12 shrink-0 rounded-md"
      />
      <div className="min-w-0 flex-1 space-y-2">
        <div className="min-w-0">
          <p className="truncate font-medium text-fg">{s.title}</p>
          <p className="truncate text-sm text-fg-muted">
            {s.user} · {s.device}
            {s.product && s.product !== s.device ? ` (${s.product})` : ""}
          </p>
        </div>
        <div className="flex flex-wrap gap-1.5">
          <Badge variant="secondary">{s.protocol}</Badge>
          <Badge variant={s.decision === "transcode" ? "warning" : "success"}>
            {decisionLabel(s.decision)}
          </Badge>
          {s.remote ? <Badge variant="info">{t("Remote")}</Badge> : null}
          {s.failovers ? (
            <Badge variant="warning">
              {tn(s.failovers, "{count} failover", "{count} failovers")}
            </Badge>
          ) : null}
          <Badge variant="secondary">{formatRate(s.speedBps)}</Badge>
        </div>
        <div className="max-w-sm">
          {s.progress !== null ? (
            <Meter
              value={s.progress}
              label={t("Started {when}", { when: relativeTime(s.startedAt) })}
              valueLabel={t("{percent}% watched", { percent: Math.round(s.progress * 100) })}
            />
          ) : (
            <p className="text-xs text-fg-muted">{t("Waiting for playback position")}</p>
          )}
        </div>
        {s.release ? (
          <p className="truncate font-mono text-[11px] text-fg-subtle" title={s.release}>
            {s.release}
          </p>
        ) : null}
      </div>
      <Button variant="outline" size="sm" disabled={stopping} onClick={onStop}>
        <Square size={14} /> {t("Stop stream")}
      </Button>
    </li>
  );
}

/** Live sessions with a stop control for each. */
export function NowPlaying({ state }: { state: AsyncState<Stream[]> }) {
  const [stopping, setStopping] = useState<string | null>(null);
  const [protocol, setProtocol] = useState("");
  const streams = state.data ?? [];
  const filtered = protocol ? streams.filter((stream) => stream.protocol === protocol) : streams;
  const protocols = [...new Set(streams.map((stream) => stream.protocol))].sort();

  const stop = async (id: string) => {
    setStopping(id);
    try {
      await apiFetch(`/stats/sessions/${encodeURIComponent(id)}/stop`, {
        method: "POST",
        body: "{}",
      });
      toast.success(t("Stream stopped"));
      await state.reload();
    } catch (e) {
      toast.error(errorMessage(e));
    } finally {
      setStopping(null);
    }
  };

  if (state.error && !state.data)
    return <ErrorPanel error={state.error} onRetry={() => void state.reload()} />;
  return (
    <Section
      title={t("Now playing")}
      description={t(
        "Streams update every few seconds. Stopping a stream ends it on the viewer's device.",
      )}
      actions={
        state.data ? (
          <Badge variant="secondary">
            {tn(streams.length, "{count} stream", "{count} streams")}
          </Badge>
        ) : null
      }
    >
      {state.data && streams.length > 0 && (
        <label className="flex items-center gap-2 text-sm">
          {t("Protocol")}
          <select value={protocol} onChange={(event) => setProtocol(event.currentTarget.value)}>
            <option value="">{t("All protocols")}</option>
            {protocols.map((value) => (
              <option key={value} value={value}>
                {value}
              </option>
            ))}
          </select>
        </label>
      )}
      {!state.data ? (
        <LoadingRows rows={2} />
      ) : filtered.length ? (
        <ul className="divide-y divide-border">
          {filtered.map((s) => (
            <StreamRow
              key={s.id || s.key}
              s={s}
              stopping={stopping === s.id}
              onStop={() => void stop(s.id)}
            />
          ))}
        </ul>
      ) : streams.length ? (
        <p>{t("No streams match this protocol.")}</p>
      ) : (
        <EmptyState
          icon={<Radio />}
          title={t("Nothing is playing")}
          description={t("Streams appear here as soon as a viewer presses play.")}
        />
      )}
    </Section>
  );
}
