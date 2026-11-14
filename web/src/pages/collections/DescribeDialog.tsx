import { AgentPanel, type AgentMessage } from "@braedonsaunders/appkit-ai/react";
import { Alert, AlertDescription, AlertTitle, Button, toast } from "@braedonsaunders/appkit-ui";
import { Sparkles } from "lucide-react";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { Drawer } from "../../components/Drawer";
import { Segmented } from "../../components/fields";
import {
  agentPanelLabels,
  cancelAgentTurn,
  deleteAgentThread,
  fetchAgentMessages,
  fetchAgentStatus,
} from "../../components/agent/agentApi";
import { localized, t, tx } from "../../i18n";
import { useLocale } from "../../i18n/react";
import { api, ApiError, apiStream, errorMessage } from "../../lib/api";
import type { CollectionSpec } from "../../lib/contract";
import { normalizeSpec } from "../../lib/rules";
import { readAgentStream } from "../plugins/LiveStage";
import { CatalogBrowser } from "../libraries/browser/Catalog";
import { TitleInspector } from "../libraries/browser/TitleInspector";
import { DescribeSurface, fillComposer } from "./DescribeSurface";
import { DescribeWall } from "./DescribeWall";

/** The agent route whose turns build one collection and outlive a reload. */
const ROUTE = "/collections/describe";
/** The conversation a reload rejoins. */
const THREAD_KEY = "ether-describe-thread";
const POLL_MS = 1500;

type Media = "movie" | "show" | "mixed";

const EXAMPLES = localized(() => [
  t("Cosy British mysteries under two hours"),
  t("Mind-bending sci-fi from the 90s"),
  t("Acclaimed animated films the whole family will like"),
  t("Slow-burn horror with great reviews"),
]);

interface ToolStep {
  name: string;
  input: Record<string, unknown>;
  output?: Record<string, unknown>;
}

type Entry = { intent: string } | { tool: ToolStep };

/** The owner's asks and the finished tool calls of a stored transcript, in order. */
function transcriptEntries(messages: AgentMessage[]): Entry[] {
  const out: Entry[] = [];
  for (const message of messages) {
    for (const raw of message.parts) {
      const part = (raw ?? {}) as Record<string, unknown>;
      const type = typeof part.type === "string" ? part.type : "";
      if (message.role === "user" && type === "text" && typeof part.text === "string") {
        out.push({ intent: part.text });
      } else if (message.role === "assistant" && part.output && typeof part.output === "object") {
        const name =
          type === "dynamic-tool"
            ? String(part.toolName ?? "")
            : type.startsWith("tool-")
              ? type.slice(5)
              : "";
        if (name)
          out.push({
            tool: {
              name,
              input: (part.input ?? {}) as Record<string, unknown>,
              output: part.output as Record<string, unknown>,
            },
          });
      }
    }
  }
  return out;
}

function draftOf(step: ToolStep): CollectionSpec | null {
  const spec = step.output?.spec as Partial<CollectionSpec> | undefined;
  return step.name === "collection_draft" && spec ? normalizeSpec(spec) : null;
}

/** The latest draft in a stored transcript. */
function latestDraft(messages: AgentMessage[]): CollectionSpec | null {
  let spec: CollectionSpec | null = null;
  for (const e of transcriptEntries(messages)) if ("tool" in e) spec = draftOf(e.tool) ?? spec;
  return spec;
}

/** Finished tool calls in a transcript; a change means the stage has news. */
function toolCount(messages: AgentMessage[]): number {
  return transcriptEntries(messages).filter((e) => "tool" in e).length;
}

/**
 * "Describe it": the owner says what they want and watches Ether build it.
 * The model's words and steps stream into the conversation, each draft
 * ticks off its filters on the stage and its posters flow into place. The
 * build runs on the server, so a reload or a closed drawer rejoins it.
 * Nothing is saved until the owner saves, opens the draft in the editor,
 * or discards it.
 */
export function DescribeDialog({
  open,
  onClose,
  onApply,
  applyLabel = t("Open in editor"),
  onSaved,
  secondary,
}: {
  open: boolean;
  onClose: () => void;
  onApply: (spec: CollectionSpec) => void;
  applyLabel?: string;
  /** Offers one-click save; called with the stored collection. */
  onSaved?: (saved: CollectionSpec) => void;
  /** A second way to use the draft, beside applyLabel. */
  secondary?: { label: string; onApply: (spec: CollectionSpec) => void };
}) {
  const { locale } = useLocale();
  const [enabled, setEnabled] = useState<boolean | null>(null);
  const [messages, setMessages] = useState<AgentMessage[]>([]);
  const [spec, setSpec] = useState<CollectionSpec | null>(null);
  const [media, setMedia] = useState<Media>("movie");
  const [streaming, setStreaming] = useState(false);
  const [rejoined, setRejoined] = useState(false);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const thread = useRef<string | undefined>(undefined);
  const turn = useRef<string | null>(null);
  const closing = useRef(false);
  const seen = useRef(0);

  const reset = () => {
    thread.current = undefined;
    turn.current = null;
    seen.current = 0;
    sessionStorage.removeItem(THREAD_KEY);
    setMessages([]);
    setSpec(null);
    setRejoined(false);
    setStreaming(false);
    setError("");
  };

  /** Reads the stored conversation; picks up the latest draft when a step finished. */
  const sync = useCallback(async (id: string) => {
    const page = await fetchAgentMessages(id);
    if (thread.current !== id) return;
    setMessages(page.messages);
    const count = toolCount(page.messages);
    if (count !== seen.current || !page.running) {
      seen.current = count;
      setSpec(latestDraft(page.messages));
    }
    setRejoined(page.running);
  }, []);

  // Opening the drawer rejoins the last build, finished or still running.
  useEffect(() => {
    if (!open) return;
    closing.current = false;
    let live = true;
    void fetchAgentStatus()
      .then((s) => live && setEnabled(s.enabled))
      .catch(() => live && setEnabled(false));
    const saved = sessionStorage.getItem(THREAD_KEY);
    if (saved && !streaming) {
      thread.current = saved;
      void sync(saved).catch((e) => {
        if (!live) return;
        if (e instanceof ApiError && e.status === 404) reset();
        else setError(errorMessage(e));
      });
    }
    return () => {
      live = false;
    };
  }, [open]);

  // A build started before a reload keeps running on the server; follow it.
  useEffect(() => {
    if (!open || !rejoined || streaming || !thread.current) return;
    const id = thread.current;
    const timer = window.setInterval(() => {
      void sync(id).catch((e) => setError(errorMessage(e)));
    }, POLL_MS);
    return () => window.clearInterval(timer);
  }, [open, rejoined, streaming, sync]);

  const follow = async (body: ReadableStream<Uint8Array>, signal: AbortSignal) => {
    const calls = new Map<string, { name: string; input: Record<string, unknown> }>();
    try {
      await readAgentStream(new Response(body), (c) => {
        if (c.type === "start" && c.messageId) turn.current = c.messageId;
        else if (c.type === "tool-input-available" && c.toolCallId && c.toolName)
          calls.set(c.toolCallId, { name: c.toolName, input: c.input ?? {} });
        else if (c.type === "tool-output-available" && c.toolCallId) {
          const call = calls.get(c.toolCallId);
          if (!call) return;
          const draft = draftOf({ ...call, output: c.output });
          if (draft) setSpec(draft);
        } else if (c.type === "error" && c.errorText) setError(c.errorText);
      });
    } catch (e) {
      if (!signal.aborted) setError(errorMessage(e));
    }
  };

  const send = async (prompt: string, signal: AbortSignal): Promise<Response> => {
    setError("");
    const res = await apiStream("/agent/turn", {
      method: "POST",
      signal,
      body: JSON.stringify({
        threadId: thread.current,
        prompt,
        context: { route: ROUTE, mediaType: media },
      }),
    });
    const id = res.headers.get("X-Ether-Thread-Id");
    if (id) {
      thread.current = id;
      sessionStorage.setItem(THREAD_KEY, id);
    }
    if (!res.ok || !res.body) return res;
    setStreaming(true);
    // The build is not tied to this request: Stop cancels it on the server,
    // while closing the drawer leaves it running to rejoin.
    const stop = () => {
      if (!closing.current && turn.current) void cancelAgentTurn(turn.current).catch(() => {});
    };
    signal.addEventListener("abort", stop, { once: true });
    const [mine, panel] = res.body.tee();
    void follow(mine, signal).finally(() => {
      signal.removeEventListener("abort", stop);
      turn.current = null;
      setStreaming(false);
      if (id && !signal.aborted) void sync(id).catch(() => {});
    });
    return new Response(panel, { status: res.status, headers: res.headers });
  };

  const busy = streaming || rejoined;
  const [openTitle, setOpenTitle] = useState<string | null>(null);
  const [browsing, setBrowsing] = useState(false);
  const [browseParams, setBrowseParams] = useState(() => new URLSearchParams());
  const chat = useRef<HTMLDivElement>(null);
  const lastAssistant = [...messages].reverse().find((m) => m.role === "assistant");

  /** Ends the conversation: the collection (or nothing) is the outcome. */
  const finish = (cancel: boolean) => {
    const id = thread.current;
    const running = cancel ? (turn.current ?? (rejoined ? lastAssistant?.id : null)) : null;
    closing.current = true;
    reset();
    onClose();
    void (async () => {
      if (running) await cancelAgentTurn(running).catch(() => {});
      if (id) await deleteAgentThread(id).catch(() => {});
    })();
  };

  const save = async () => {
    if (!spec || !onSaved) return;
    setSaving(true);
    setError("");
    try {
      const saved = await api.createCollection(spec);
      toast.success(t("Saved “{name}”", { name: saved.name }));
      onSaved(saved);
      finish(false);
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setSaving(false);
    }
  };

  const labels = useMemo(
    () => ({
      ...agentPanelLabels(),
      title: t("Build with Ether"),
      disabledDescription: t("Connect a language model to describe collections."),
      placeholder: t("e.g. Feel-good 80s adventure films for a Saturday night"),
    }),

    [locale],
  );
  const toolLabels = useMemo(
    () => ({
      collection_draft: t("Draft the collection"),
      catalog_search: t("Search the catalogue"),
      title_details: t("Title details"),
    }),

    [locale],
  );

  return (
    <DescribeSurface
      open={open}
      onClose={() => {
        closing.current = true;
        onClose();
      }}
      title={
        <span className="flex items-center gap-2">
          <Sparkles size={18} className="text-primary" /> {t("Describe a collection")}
        </span>
      }
      description={t(
        "Say what you want in plain English. Ether drafts the sources and rules; you review them before anything is saved.",
      )}
      footer={
        <div className="flex w-full flex-wrap items-center justify-end gap-2">
          {busy ? (
            <span className="mr-auto text-xs text-fg-muted" aria-live="polite">
              {t("Ether is building. You can close this and come back.")}
            </span>
          ) : null}
          <Button variant="ghost" onClick={() => finish(true)}>
            {spec || busy ? t("Discard") : t("Cancel")}
          </Button>
          {secondary ? (
            <Button
              variant="outline"
              disabled={!spec || busy}
              onClick={() => {
                if (!spec) return;
                secondary.onApply(spec);
                finish(false);
              }}
            >
              {secondary.label}
            </Button>
          ) : null}
          <Button
            variant={onSaved ? "outline" : undefined}
            disabled={!spec || busy}
            onClick={() => {
              if (!spec) return;
              onApply(spec);
              finish(false);
            }}
          >
            {applyLabel}
          </Button>
          {onSaved ? (
            <Button disabled={!spec || busy || saving} onClick={() => void save()}>
              {saving ? t("Saving…") : t("Save collection")}
            </Button>
          ) : null}
        </div>
      }
      preview={
        <>
          {enabled === false ? (
            <Alert variant="info" className="m-4">
              <AlertTitle>{t("Describe it needs a language model")}</AlertTitle>
              <AlertDescription>
                {tx(
                  "Connect an OpenAI-compatible endpoint (a local Ollama works) in {integrations}, then try again. You can still build collections by hand or from a template.",
                  {
                    integrations: (
                      <Link to="/services" className="font-medium underline" onClick={onClose}>
                        {t("Integrations")}
                      </Link>
                    ),
                  },
                )}
              </AlertDescription>
            </Alert>
          ) : (
            <DescribeWall
              spec={spec}
              building={busy}
              examples={EXAMPLES}
              onExample={(text) => fillComposer(chat.current, text)}
              onOpenTitle={setOpenTitle}
              onShowMore={() => {
                setBrowseParams(new URLSearchParams());
                setBrowsing(true);
              }}
            />
          )}
        </>
      }
      error={error}
    >
      <div ref={chat} className="contents">
        <AgentPanel
          enabled={enabled !== false}
          initialMessages={messages}
          labels={labels}
          toolLabels={toolLabels}
          working={rejoined && !streaming}
          send={send}
          emptyContent={
            <p className="m-auto max-w-md px-6 text-center text-sm text-fg-muted">
              {t("Describe the collection below, or pick an idea above.")}
            </p>
          }
          composerActions={
            // The composer row aligns to its bottom; centre the switch on the input line.
            <div className="flex h-10 shrink-0 items-center">
              <Segmented
                ariaLabel={t("Media type")}
                size="sm"
                value={media}
                onChange={setMedia}
                options={[
                  { value: "movie", label: t("Movies") },
                  { value: "show", label: t("TV") },
                  { value: "mixed", label: t("Both") },
                ]}
              />
            </div>
          }
        />
      </div>
      <Drawer
        open={browsing && spec !== null}
        onClose={() => setBrowsing(false)}
        title={spec?.name || t("Untitled collection")}
        size="2xl"
      >
        {browsing && spec ? (
          <CatalogBrowser
            params={browseParams}
            setParams={setBrowseParams}
            refreshHealth={async () => {}}
            collection={spec}
          />
        ) : null}
      </Drawer>
      {openTitle ? (
        <TitleInspector key={openTitle} titleKey={openTitle} onClose={() => setOpenTitle(null)} />
      ) : null}
    </DescribeSurface>
  );
}
