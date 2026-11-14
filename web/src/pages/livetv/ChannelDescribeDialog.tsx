import type { AgentMessage } from "@braedonsaunders/appkit-ai/react";
import { Button, Spinner } from "@braedonsaunders/appkit-ui";
import { Radio, Sparkles } from "lucide-react";
import { type ReactNode, useCallback, useEffect, useMemo, useRef, useState } from "react";
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
import { ScenePoster, styleBindings } from "./onair/Stage";
import type { Look } from "./onair/model";
import { tv, type Programme, type Channel } from "./model";
import { Artwork } from "../../components/Artwork";
import { programmeArtwork } from "./model";
import "./channel-describe.css";
import "../collections/describe.css";
import "../plugins/stage.css";
import { ScheduleTimeline, type TimelineResponse } from "./ScheduleTimeline";
function SchedulePreview({ channel, hero }: { channel: Channel; hero: ReactNode }) {
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
    if (
      !channel.items.length &&
      !channel.filter &&
      !channel.collection &&
      !channel.sources?.length &&
      !channel.blocks?.length &&
      !channel.personal
    )
      return () => ctl.abort();
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
          dayparts: (channel.blocks ?? []).map((block, i) => ({
            id: String(i),
            name: block.name || "",
            start: block.start,
            end: block.end,
            days: block.days?.length ? block.days : [0, 1, 2, 3, 4, 5, 6],
          })),
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
  return (
    <div className="grid items-center gap-3 sm:grid-cols-[1.4fr_1fr]">
      {hero}
      <div className="min-w-0">
        {data ? (
          <div className="flex gap-3 overflow-hidden">
            {programmes.map((p) => (
              <div key={p.id} className="stage-flow w-16 shrink-0">
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
        ) : (
          <div aria-hidden className="flex gap-3 overflow-hidden">
            {Array.from({ length: 8 }, (_, i) => (
              <div
                key={i}
                className="describe-ghost h-28 w-20 shrink-0"
                style={{ animationDelay: `${i * 100}ms` }}
              />
            ))}
          </div>
        )}
      </div>
      <div className="min-w-0 sm:col-span-2">
        {error ? (
          <p role="alert">{error}</p>
        ) : data ? (
          <ScheduleTimeline
            channelId={channel.id || "draft"}
            snapshot={data}
            range={range}
            showLegend={false}
          />
        ) : (
          <div aria-hidden className="flex gap-2">
            {[2, 3, 2, 1].map((n, i) => (
              <div key={i} className="describe-ghost h-16" style={{ flex: n }} />
            ))}
          </div>
        )}
      </div>
    </div>
  );
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
  const [applied, setApplied] = useState(false);
  const [started, setStarted] = useState(false);
  const [transcriptRevision, setTranscriptRevision] = useState(0);
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
      setTranscriptRevision((revision) => revision + 1);
    }
  }, []);
  // AppKit adopts host messages in an effect. Wait for that render before
  // revealing the approval card, which arrives after the live stream ends.
  // History pagination deliberately does not increment this revision.
  useEffect(() => {
    if (!transcriptRevision) return;
    let frame = requestAnimationFrame(() => {
      frame = requestAnimationFrame(() => {
        const viewport = chat.current?.querySelector<HTMLElement>(".app-scroll");
        if (viewport) viewport.scrollTop = viewport.scrollHeight;
      });
    });
    return () => cancelAnimationFrame(frame);
  }, [transcriptRevision]);
  const send = async (prompt: string, signal: AbortSignal) => {
    setError("");
    setStarted(true);
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
      roomyChat
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
      footer={
        <Button disabled={!applied || busy} onClick={onClose}>
          {t("Continue editing")}
        </Button>
      }
      preview={
        <div className={`channel-build p-4 ${busy ? "describe-building" : ""}`}>
          <SchedulePreview
            channel={proposal}
            hero={
              <div
                className="channel-build-hero relative overflow-hidden rounded-2xl border border-border bg-surface shadow-lg"
                style={{
                  background: `radial-gradient(ellipse at 80% 0%, ${style?.brand.palette.accent || "#7960ed"}33, transparent 65%), ${style?.brand.palette.background || "#121323"}`,
                  color: style?.brand.palette.text || "#f8f8ff",
                }}
              >
                <div className="channel-build-glow" aria-hidden />
                <div className="relative grid items-center gap-3 p-4 sm:grid-cols-[1fr_120px]">
                  <div className="min-w-0">
                    <div className="mb-2 flex items-center gap-2 text-xs font-medium uppercase tracking-widest opacity-70">
                      <Radio size={14} /> {t("Channel {number}", { number: proposal.number })}
                      <span className="ml-auto flex items-center gap-1.5 normal-case tracking-normal">
                        <span
                          className={`size-1.5 rounded-full bg-current ${busy ? "animate-pulse" : ""}`}
                        />
                        {busy ? t("Building…") : t("Live preview")}
                      </span>
                    </div>
                    <h3 className="truncate text-xl font-semibold tracking-tight">
                      {proposal.name || t("New channel")}
                    </h3>
                    <p className="mt-1 text-sm opacity-70">
                      {style?.brand.tagline ||
                        t("Programming, schedule and a look, built with you.")}
                    </p>
                    <div className="mt-3 flex items-center gap-2">
                      {style ? (
                        <>
                          {Object.values(style.brand.palette).map((color, i) => (
                            <span
                              key={i}
                              className="size-4 rounded-full ring-1 ring-white/30"
                              style={{ background: color }}
                            />
                          ))}
                          <span className="ml-1 truncate text-xs opacity-70">{style.name}</span>
                        </>
                      ) : (
                        <span className="text-xs opacity-50">
                          {t("Your channel takes shape here")}
                        </span>
                      )}
                    </div>
                  </div>
                  <div className="hidden overflow-hidden rounded-xl border border-white/10 bg-black/20 shadow-lg sm:block">
                    {style?.scenes.some((s) => s.enabled) ? (
                      <DraftBrand look={style} channel={proposal} />
                    ) : (
                      <div className="relative grid aspect-video place-items-center">
                        <div className="channel-build-orbit absolute size-24 rounded-full border border-white/15" />
                        <Radio size={32} className="opacity-50" />
                      </div>
                    )}
                  </div>
                </div>
              </div>
            }
          />
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
          <>
            {!started && (
              <div className="flex shrink-0 flex-wrap gap-1.5 border-b border-border px-4 py-2">
                {examples.map((text) => (
                  <button
                    key={text}
                    type="button"
                    disabled={busy}
                    onClick={() => fillComposer(chat.current, text)}
                    className="rounded-full border border-border px-2.5 py-1 text-xs text-fg-muted transition-colors hover:border-primary/50 hover:text-fg disabled:opacity-50"
                  >
                    {text}
                  </button>
                ))}
              </div>
            )}
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
                  setApplied(true);
                }
                await sync();
              }}
            />
          </>
        )}
      </div>
    </DescribeSurface>
  );
}

/** Only the open builder animates; reduced-motion owners get the scene poster. */
function DraftBrand({ look, channel }: { look: Look; channel: Channel }) {
  const [playing, setPlaying] = useState(false);
  useEffect(() => {
    const query = window.matchMedia("(prefers-reduced-motion: reduce)");
    const update = () => setPlaying(!query.matches && !document.hidden);
    update();
    query.addEventListener("change", update);
    document.addEventListener("visibilitychange", update);
    return () => {
      query.removeEventListener("change", update);
      document.removeEventListener("visibilitychange", update);
    };
  }, []);
  const scene =
    look.scenes.find((s) => s.role === "ident" && s.enabled) || look.scenes.find((s) => s.enabled);
  const bindings = useMemo(
    () => styleBindings(look, { channel: { name: channel.name, logo: channel.logo } }),
    [look, channel.name, channel.logo],
  );
  return scene ? <ScenePoster scene={scene} bindings={bindings} playing={playing} /> : null;
}
