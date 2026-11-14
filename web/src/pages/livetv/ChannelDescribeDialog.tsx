import type { AgentMessage } from "@braedonsaunders/appkit-ai/react";
import { Button, Spinner } from "@braedonsaunders/appkit-ui";
import { Sparkles } from "lucide-react";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link } from "react-router-dom";
import AskEtherPanel from "../../components/agent/AskEtherPanel";
import {
  agentPanelLabels,
  decideAgentApproval,
  fetchAgentMessages,
  fetchAgentStatus,
  cancelAgentTurn,
} from "../../components/agent/agentApi";
import { approvalRequestLabels } from "../../components/agent/approvalLabels";
import { DescribeSurface, fillComposer } from "../collections/DescribeSurface";
import { readAgentStream } from "../plugins/LiveStage";
import { t } from "../../i18n";
import { useLocale } from "../../i18n/react";
import { apiStream, errorMessage } from "../../lib/api";
import { LookTile } from "./onair/Stage";
import type { Look } from "./onair/model";
import { tv, type Programme, type Channel } from "./model";
import { Artwork } from "../../components/Artwork";
import { programmeArtwork } from "./model";
import { ScheduleTimeline, type TimelineResponse } from "./ScheduleTimeline";
export function SchedulePreview({ channel }: { channel: Channel }) {
  const [programmes, setProgrammes] = useState<Programme[]>([]);
  const [data, setData] = useState<TimelineResponse>();
  const [error, setError] = useState("");
  const [range] = useState(() => ({ from: Date.now(), to: Date.now() + 6 * 3600000 }));
  const key = JSON.stringify(channel);
  useEffect(() => {
    const ctl = new AbortController();
    setData(undefined);
    setProgrammes([]);
    setError("");
    tv.preview(JSON.parse(key) as Channel, ctl.signal, {
      from: new Date(range.from),
      until: new Date(range.to),
    })
      .then((programmes: Programme[]) => {
        if (ctl.signal.aborted) return;
        setProgrammes(programmes.filter((p) => !p.slate && p.item.key).slice(0, 8));
        setData({
          channelId: channel.id || "draft",
          timezone: channel.timezone || "UTC",
          now: range.from,
          dayparts: [],
          elements: programmes
            .filter((p) => !p.slate)
            .map((p) => ({
              id: p.id,
              kind: "programme",
              start: Date.parse(p.start),
              end: Date.parse(p.end),
              title: p.item.title,
              rule: p.reason || "",
            })),
        });
      })
      .catch((e) => {
        if (!ctl.signal.aborted) setError(errorMessage(e));
      });
    return () => ctl.abort();
  }, [key, range]);
  return error ? (
    <p role="alert">{error}</p>
  ) : data ? (
    <div className="space-y-4">
      <div className="grid grid-cols-4 gap-3 sm:grid-cols-6">
        {programmes.map((p) => (
          <div key={p.id} className="min-w-0">
            <Artwork
              src={programmeArtwork(p.item.thumb)}
              alt=""
              title={p.item.title}
              className="aspect-[2/3] overflow-hidden rounded-lg bg-bg-subtle"
              imgClassName="object-cover"
            />
            <p className="mt-1 truncate text-xs text-fg-muted">{p.item.title}</p>
          </div>
        ))}
      </div>
      <ScheduleTimeline channelId={channel.id || "draft"} snapshot={data} range={range} />
    </div>
  ) : null;
}

const ROUTE = "/livetv/channel-builder";
export function ChannelDescribeDialog({
  channel,
  onApply,
  onClose,
}: {
  channel: Channel;
  onApply: (channel: Channel) => void;
  onClose: () => void;
}) {
  const { locale } = useLocale();
  const [enabled, setEnabled] = useState<boolean | null>(null);
  const [messages, setMessages] = useState<AgentMessage[]>([]);
  const [proposal, setProposal] = useState<Channel>(channel);
  const [hasOlder, setHasOlder] = useState(false);
  const [style, setStyle] = useState<Look>();
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const thread = useRef<string | undefined>(undefined);
  const turn = useRef<string | undefined>(undefined);
  const chat = useRef<HTMLDivElement>(null);
  const active = useRef<AbortController | null>(null);
  const current = useRef(channel);
  const alive = useRef(true);
  useEffect(() => {
    alive.current = true;
    void fetchAgentStatus()
      .then((s) => {
        if (alive.current) setEnabled(s.enabled);
      })
      .catch((e) => {
        if (alive.current) setError(errorMessage(e));
      });
    return () => {
      alive.current = false;
      active.current?.abort();
      if (turn.current) void cancelAgentTurn(turn.current).catch(() => {});
    };
  }, []);
  const sync = useCallback(async () => {
    if (!thread.current) return;
    const page = await fetchAgentMessages(thread.current);
    if (alive.current) {
      setMessages(page.messages);
      setHasOlder(page.hasOlder);
    }
  }, []);
  const send = async (prompt: string, signal: AbortSignal) => {
    setError("");
    const ctl = new AbortController();
    active.current = ctl;
    const stop = () => {
      ctl.abort();
      if (turn.current) void cancelAgentTurn(turn.current).catch(() => {});
    };
    signal.addEventListener("abort", stop, { once: true });
    try {
      const res = await apiStream("/agent/turn", {
        method: "POST",
        signal: ctl.signal,
        body: JSON.stringify({
          threadId: thread.current,
          prompt,
          context: { route: ROUTE, draft: { editor: true, channel: current.current } },
        }),
      });
      thread.current = res.headers.get("X-Ether-Thread-Id") ?? thread.current;
      if (!res.ok || !res.body) {
        signal.removeEventListener("abort", stop);
        return res;
      }
      setBusy(true);
      const [mine, panel] = res.body.tee();
      // Watch the same stream as the chat so the preview changes as tools finish.
      void readAgentStream(new Response(mine), (c) => {
        if (!alive.current) return;
        if (c.type === "start") turn.current = c.messageId;
        if (c.type === "tool-output-available" && c.output) {
          const output = c.output as { channel?: Channel; style?: Look; look?: Look };
          if (output.channel) setProposal(output.channel);
          if (output.style?.scenes) setStyle(output.style);
          if (output.look?.scenes) setStyle(output.look);
        }
      })
        .catch((e) => {
          if (alive.current && !ctl.signal.aborted) setError(errorMessage(e));
        })
        .finally(() => {
          signal.removeEventListener("abort", stop);
          turn.current = undefined;
          if (alive.current) {
            setBusy(false);
            void sync().catch((e) => setError(errorMessage(e)));
          }
        });
      return new Response(panel, { status: res.status, headers: res.headers });
    } catch (e) {
      signal.removeEventListener("abort", stop);
      throw e;
    }
  };
  const labels = useMemo(
    () => ({
      ...agentPanelLabels(),
      title: t("Describe it"),
      welcomeTitle: t("What should it play?"),
      welcomeDescription: t(
        "Describe the programming. Apply updates this draft; save the channel when you’re ready.",
      ),
      placeholder: t("e.g. Feel-good 80s adventure films for a Saturday night"),
    }),
    [locale],
  );
  const examples = [
    t("Cosy British mysteries under two hours"),
    t("Mind-bending sci-fi from the 90s"),
    t("Acclaimed animated films the whole family will like"),
  ];
  return (
    <DescribeSurface
      open
      onClose={onClose}
      title={
        <span className="flex items-center gap-2">
          <Sparkles size={18} className="text-primary" />
          {t("Describe it")}
        </span>
      }
      description={t(
        "Describe the programming. Apply updates this draft; save the channel when you’re ready.",
      )}
      error={error}
      preview={
        <div className="space-y-4 p-4">
          <div className="flex items-center gap-2">
            <h3 className="flex-1 text-lg font-semibold">{proposal.name || t("New channel")}</h3>
            <span className="text-xs text-fg-muted">
              {busy ? t("Building…") : t("Live preview")}
            </span>
          </div>
          {style?.scenes && <LookTile look={style} playOnHover />}
          <SchedulePreview channel={proposal} />
          <div className="flex flex-wrap gap-2">
            {examples.map((text) => (
              <Button
                key={text}
                variant="outline"
                size="sm"
                disabled={busy}
                onClick={() => fillComposer(chat.current, text)}
              >
                {text}
              </Button>
            ))}
          </div>
        </div>
      }
    >
      <div ref={chat} className="contents [&>div>header]:hidden">
        {enabled === null ? (
          <Spinner label={t("Loading")} />
        ) : !enabled ? (
          <div className="p-4">
            <p>{t("Describe it needs a language model")}</p>
            <Link className="underline" to="/services/llm" onClick={onClose}>
              {t("Open Language model settings")}
            </Link>
          </div>
        ) : (
          <AskEtherPanel
            route={ROUTE}
            messages={messages}
            labels={labels}
            approvalRequestLabels={{
              ...approvalRequestLabels(),
              approve: t("Apply"),
              approved: t("Applied"),
            }}
            secretRequestLabels={{}}
            send={send}
            hasOlderMessages={hasOlder}
            onLoadOlderMessages={async () => {
              if (!thread.current) return;
              const page = await fetchAgentMessages(thread.current, messages[0]?.id);
              setMessages((previous) => [...page.messages, ...previous]);
              setHasOlder(page.hasOlder);
            }}
            onSubmitSecretRequest={async () => {}}
            onDecideApprovalRequest={async (id, decision, note) => {
              const result = await decideAgentApproval(id, decision, note, { route: ROUTE });
              if (result.status === "failed")
                throw new Error(result.failureReason || t("Could not apply the draft"));
              if (decision === "approved" && result.result?.channel) {
                const next = { ...result.result.channel, id: channel.id };
                current.current = next;
                setProposal(next);
                onApply(next);
              }
              await sync();
            }}
          />
        )}
      </div>
    </DescribeSurface>
  );
}
