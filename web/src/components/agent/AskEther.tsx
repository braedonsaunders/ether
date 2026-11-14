/**
 * Ask Ether: the top-bar agent button and drawer (docs/extend/SPEC.md WS6).
 *
 * AppKit's AgentPanel owns the transcript and composer. This component owns
 * the wire: turns post to /api/agent/turn, and the durable transcript is
 * refetched after every turn and every approval or secret decision, so
 * approval-request and secret-request parts (which the panel only renders
 * from initialMessages) appear without duplicating the streamed answer.
 * AgentPanel reconciles a settled panel against the host transcript only
 * once the host has as many assistant turns as the panel has streamed, so
 * handing it the refetched messages is flicker-free. Switching, clearing
 * or starting a conversation remounts the panel instead, because that
 * floor would otherwise keep the old transcript alive.
 */
import type { AgentMessage } from "@braedonsaunders/appkit-ai/react";
import { Spinner, buttonVariants, toast } from "@braedonsaunders/appkit-ui";
import { History, Plug, Plus, Sparkles, Trash2 } from "lucide-react";
import { lazy, Suspense, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link, useLocation, useNavigate } from "react-router-dom";
import { t } from "../../i18n";
import { useLocale } from "../../i18n/react";
import { formatDate } from "../../lib/format";
import { apiStream, errorMessage } from "../../lib/api";
import { ActionMenu } from "../ActionMenu";
import { Drawer } from "../Drawer";
import { ErrorPanel } from "../feedback";
import { approvalRequestLabels as approvalLabels } from "./approvalLabels";
import {
  agentPanelLabels,
  decideAgentApproval,
  deleteAgentThread,
  extractNavigateRoute,
  fetchAgentMessages,
  fetchAgentStatus,
  fetchAgentThreads,
  inferRecord,
  submitAgentSecret,
  type AgentStatus,
  type AgentThreadSummary,
} from "./agentApi";
import { McpPairingDrawer } from "./McpPairing";

// The panel pulls in the AI SDK stream parser and the markdown renderer, so
// it loads only when the owner opens the drawer.
const AskEtherPanel = lazy(() => import("./AskEtherPanel"));

export function AskEther() {
  const location = useLocation();
  const navigate = useNavigate();
  const { locale } = useLocale();
  const [open, setOpen] = useState(false);
  const [status, setStatus] = useState<AgentStatus | null>(null);
  const [statusError, setStatusError] = useState<unknown>(null);
  const [threadId, setThreadId] = useState<string | undefined>(undefined);
  const [messages, setMessages] = useState<AgentMessage[]>([]);
  const [hasOlder, setHasOlder] = useState(false);
  const [panelEpoch, setPanelEpoch] = useState(0);
  const [historyOpen, setHistoryOpen] = useState(false);
  const [pairingOpen, setPairingOpen] = useState(false);
  const [threads, setThreads] = useState<AgentThreadSummary[] | null>(null);
  const [threadsError, setThreadsError] = useState<unknown>(null);

  const threadIdRef = useRef<string | undefined>(undefined);
  const navigateArmedRef = useRef(false);
  const seenNavigateRef = useRef<Set<string>>(new Set());
  const route = location.pathname;
  const title = t("Ask Ether");

  const setCurrentThread = useCallback((id: string | undefined) => {
    threadIdRef.current = id;
    setThreadId(id);
  }, []);

  // locale re-renders the copy in the active language.

  const labels = useMemo(() => agentPanelLabels(), [locale]);

  const approvalRequestLabels = useMemo(() => approvalLabels(), [locale]);

  const secretRequestLabels = useMemo(
    () => ({
      show: t("Show credential"),
      hide: t("Hide credential"),
      submit: t("Save securely"),
      cancel: t("Cancel"),
      help: t("Open setup instructions"),
      submitting: t("Saving securely"),
      stored: t("Credential saved securely."),
      expired: t("This credential request has expired."),
      required: t("Enter the credential before saving."),
      failed: t("The credential could not be saved. Enter it again and try again."),
      cancelFailed: t("The credential request could not be cancelled. Try again."),
    }),
    [locale],
  );

  const maybeNavigate = useCallback(
    (next: AgentMessage[]) => {
      if (!navigateArmedRef.current) return;
      const found = extractNavigateRoute(next, seenNavigateRef.current);
      if (!found) return;
      seenNavigateRef.current.add(found.key);
      navigateArmedRef.current = false;
      setOpen(false);
      navigate(found.route);
    },
    [navigate],
  );

  const refetchMessages = useCallback(
    async (id: string) => {
      const page = await fetchAgentMessages(id);
      setMessages(page.messages);
      setHasOlder(page.hasOlder);
      maybeNavigate(page.messages);
      return page;
    },
    [maybeNavigate],
  );

  const loadStatus = useCallback(async () => {
    setStatusError(null);
    try {
      setStatus(await fetchAgentStatus());
    } catch (e) {
      setStatusError(e);
      setStatus(null);
    }
  }, []);

  // Status is checked on every open, and the durable transcript is
  // refreshed so a pending approval raised elsewhere is waiting inside.
  useEffect(() => {
    if (!open) return;
    void loadStatus();
    const id = threadIdRef.current;
    if (id) void refetchMessages(id).catch(() => undefined);
  }, [open, loadStatus, refetchMessages]);

  // Move focus to the composer once the drawer and panel have settled.
  useEffect(() => {
    if (!open || !status?.enabled) return;
    const timer = window.setTimeout(() => {
      const dialog = document.querySelector('[role="dialog"]');
      // Opening can finish after the owner has already started entering a
      // credential. Never redirect those keystrokes into the visible composer.
      if (
        dialog?.querySelector("input:focus, textarea:focus, select:focus, [contenteditable]:focus")
      )
        return;
      dialog?.querySelector<HTMLTextAreaElement>("textarea")?.focus();
    }, 80);
    return () => window.clearTimeout(timer);
  }, [open, status?.enabled, panelEpoch]);

  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if (event.altKey || !(event.metaKey || event.ctrlKey)) return;
      if (event.key.toLowerCase() !== "j") return;
      event.preventDefault();
      setOpen((value) => !value);
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  const send = useCallback(
    async (prompt: string, signal: AbortSignal): Promise<Response> => {
      navigateArmedRef.current = true;
      const response = await apiStream("/agent/turn", {
        method: "POST",
        signal,
        body: JSON.stringify({
          threadId: threadIdRef.current,
          prompt,
          context: { route, record: inferRecord(route) },
        }),
      });
      const headerId = response.headers.get("X-Ether-Thread-Id");
      if (headerId) setCurrentThread(headerId);
      if (!response.ok || !response.body) return response;
      const targetThread = headerId ?? threadIdRef.current;
      let finished = false;
      const reader = response.body.getReader();
      const wrapped = new ReadableStream<Uint8Array>({
        async pull(controller) {
          try {
            const { done, value } = await reader.read();
            if (done) {
              controller.close();
              if (!finished) {
                finished = true;
                if (targetThread && !signal.aborted)
                  void refetchMessages(targetThread).catch(() => undefined);
              }
              return;
            }
            controller.enqueue(value);
          } catch (e) {
            controller.error(e);
          }
        },
        cancel(reason) {
          return reader.cancel(reason);
        },
      });
      return new Response(wrapped, {
        status: response.status,
        statusText: response.statusText,
        headers: response.headers,
      });
    },
    [route, refetchMessages, setCurrentThread],
  );

  const onLoadOlderMessages = useCallback(async () => {
    const id = threadIdRef.current;
    if (!id) return;
    const first = messages[0]?.id;
    const page = await fetchAgentMessages(id, first);
    setMessages((current) => {
      const known = new Set(current.map((m) => m.id));
      return [...page.messages.filter((m) => !known.has(m.id)), ...current];
    });
    setHasOlder(page.hasOlder);
  }, [messages]);

  const decideApproval = useCallback(
    async (approvalId: string, decision: "approved" | "rejected", note?: string) => {
      await decideAgentApproval(approvalId, decision, note, {
        route,
        record: inferRecord(route),
      });
      const id = threadIdRef.current;
      if (id) await refetchMessages(id);
    },
    [refetchMessages, route],
  );

  const submitSecret = useCallback(
    async (requestId: string, value: string) => {
      await submitAgentSecret(requestId, value);
      const id = threadIdRef.current;
      if (id) await refetchMessages(id);
    },
    [refetchMessages],
  );

  const resetConversation = useCallback(() => {
    navigateArmedRef.current = false;
    setCurrentThread(undefined);
    setMessages([]);
    setHasOlder(false);
    setPanelEpoch((epoch) => epoch + 1);
  }, [setCurrentThread]);

  const clearConversation = useCallback(async () => {
    const id = threadIdRef.current;
    resetConversation();
    if (!id) return;
    try {
      await deleteAgentThread(id);
    } catch (e) {
      toast.error(t("Could not clear this conversation"), {
        description: errorMessage(e),
      });
    }
  }, [resetConversation]);

  const openHistory = useCallback(() => {
    setHistoryOpen(true);
    setThreads(null);
    setThreadsError(null);
    void fetchAgentThreads()
      .then(setThreads)
      .catch((e: unknown) => setThreadsError(e));
  }, []);

  const openThread = useCallback(
    async (id: string) => {
      try {
        const page = await fetchAgentMessages(id);
        navigateArmedRef.current = false;
        setCurrentThread(id);
        setMessages(page.messages);
        setHasOlder(page.hasOlder);
        setPanelEpoch((epoch) => epoch + 1);
        setHistoryOpen(false);
      } catch (e) {
        setThreadsError(e);
      }
    },
    [setCurrentThread],
  );

  const menuItems = useMemo(
    () => [
      {
        key: "new",
        label: t("New conversation"),
        icon: Plus,
        onSelect: () => resetConversation(),
      },
      {
        key: "earlier",
        label: t("Earlier conversations"),
        icon: History,
        onSelect: () => openHistory(),
      },
      {
        key: "clear",
        label: t("Clear this conversation"),
        icon: Trash2,
        danger: true,
        disabled: !threadId && messages.length === 0,
        onSelect: () => void clearConversation(),
      },
      { key: "sep", separator: true as const },
      {
        key: "connect",
        label: t("Connect an AI app"),
        icon: Plug,
        onSelect: () => setPairingOpen(true),
      },
    ],
    // Menu copy follows the active language.

    [resetConversation, openHistory, clearConversation, threadId, messages.length, locale],
  );

  return (
    <>
      <button
        type="button"
        onClick={() => setOpen((value) => !value)}
        aria-label={title}
        aria-keyshortcuts="Meta+J"
        aria-expanded={open}
        title={title}
        className="inline-flex h-9 shrink-0 px-2 items-center justify-center rounded-md text-fg-muted transition-colors hover:bg-surface-hover hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50"
      >
        <Sparkles size={16} aria-hidden />
      </button>

      <Drawer
        open={open}
        onClose={() => setOpen(false)}
        size="sm"
        title={
          <span className="inline-flex items-center gap-2">
            <Sparkles size={16} className="text-primary" aria-hidden />
            {title}
          </span>
        }
        headerActions={
          status?.enabled ? (
            <ActionMenu items={menuItems} label={t("Conversation options")} />
          ) : undefined
        }
        bodyClassName="flex min-h-0 flex-1 flex-col overflow-hidden p-0"
      >
        {statusError ? (
          <div className="p-4">
            <ErrorPanel error={statusError} onRetry={() => void loadStatus()} />
          </div>
        ) : status === null ? (
          <div className="grid flex-1 place-items-center p-10">
            <Spinner size={22} label={t("Loading Ask Ether")} />
          </div>
        ) : !status.enabled ? (
          <div className="flex flex-1 items-start p-4 sm:p-6">
            <section
              aria-label={t("Connect a model")}
              className="w-full rounded-xl border border-border bg-bg-subtle p-5"
            >
              <span className="flex size-9 items-center justify-center rounded-lg bg-primary-subtle text-primary">
                <Sparkles size={17} aria-hidden />
              </span>
              <h3 className="mt-3 text-base font-semibold text-fg">{t("Connect a model")}</h3>
              <p className="mt-1.5 text-sm leading-relaxed text-fg-muted">
                {t(
                  "Ask Ether answers with the language model you connect. Add one and this drawer is ready.",
                )}
              </p>
              <Link
                to="/services/llm"
                onClick={() => setOpen(false)}
                className={buttonVariants({ size: "sm", className: "mt-4" })}
              >
                {t("Open Language model settings")}
              </Link>
            </section>
          </div>
        ) : (
          <div className="flex min-h-0 flex-1 flex-col [&>div>header]:hidden">
            <Suspense
              fallback={
                <div className="grid flex-1 place-items-center">
                  <Spinner size={20} label={t("Loading")} />
                </div>
              }
            >
              <AskEtherPanel
                key={panelEpoch}
                route={route}
                messages={messages}
                labels={labels}
                approvalRequestLabels={approvalRequestLabels}
                secretRequestLabels={secretRequestLabels}
                send={send}
                onDecideApprovalRequest={decideApproval}
                onSubmitSecretRequest={submitSecret}
                hasOlderMessages={hasOlder}
                onLoadOlderMessages={onLoadOlderMessages}
              />
            </Suspense>
          </div>
        )}

        <Drawer
          open={historyOpen}
          onClose={() => setHistoryOpen(false)}
          title={t("Earlier conversations")}
          size="sm"
        >
          {threadsError ? <ErrorPanel error={threadsError} /> : null}
          {threads === null && !threadsError ? (
            <div className="grid place-items-center p-8">
              <Spinner size={20} label={t("Loading conversations")} />
            </div>
          ) : threads && threads.length === 0 ? (
            <p className="text-sm text-fg-muted">{t("No earlier conversations.")}</p>
          ) : (
            <ul className="divide-y divide-border rounded-xl border border-border">
              {(threads ?? []).map((thread) => {
                const stamp = thread.updatedAt ?? thread.createdAt;
                const date = stamp ? formatDate(stamp) : "";
                return (
                  <li key={thread.id}>
                    <button
                      type="button"
                      onClick={() => void openThread(thread.id)}
                      className="flex w-full items-center gap-3 px-3 py-2.5 text-left transition-colors hover:bg-surface-hover focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring/50"
                    >
                      <span className="min-w-0 flex-1 truncate text-sm text-fg">
                        {thread.title}
                      </span>
                      {date ? <span className="shrink-0 text-xs text-fg-muted">{date}</span> : null}
                    </button>
                  </li>
                );
              })}
            </ul>
          )}
        </Drawer>

        <McpPairingDrawer open={pairingOpen} onClose={() => setPairingOpen(false)} />
      </Drawer>
    </>
  );
}
