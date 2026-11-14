/**
 * The Ask Ether transcript and composer, loaded lazily by AskEther.tsx.
 */
import {
  AgentPanel,
  type AgentApprovalRequestLabels,
  type AgentMessage,
  type AgentPanelLabels,
  type AgentSecretRequestLabels,
} from "@braedonsaunders/appkit-ai/react";
import { useMemo, useRef, useState } from "react";
import { Button } from "@braedonsaunders/appkit-ui";
import { t } from "../../i18n";
import { useLocale } from "../../i18n/react";
import { useRegistry } from "../../lib/extend";
import { humanizeToolName, registrySuggestions, toolLabelsFromDescriptors } from "./agentApi";

const KNOWN_TOOLS = [
  "server_status",
  "health_details",
  "playback_history",
  "resolve_trace",
  "release_health",
  "logs_tail",
  "catalog_search",
  "title_details",
  "collection_list",
  "collection_preview",
  "collection_draft",
  "livetv_channels",
  "sources_list",
  "plugins_list",
  "plugin_health",
  "approval_status",
  "navigate",
  "collection_create_from_prompt",
  "release_blocklist",
  "release_pin",
  "quality_profiles",
  "quality_order_set",
  "format_scores_set",
  "rule_add",
  "source_add_stremio",
  "source_add_m3u",
  "frontend_enable",
  "plugin_enable",
  "server_restart",
];

/** Page defaults used until WS1d serves agentSuggestion command descriptors. */
function defaultSuggestion(route: string): string {
  if (route.startsWith("/releases")) return t("Why did this title pick this release?");
  if (route.startsWith("/livetv")) return t("Which channels are failing?");
  if (route.startsWith("/sources")) return t("Add a plugin from Stremio");
  return t("What needs my attention?");
}

/**
 * The panel plus its registry-driven suggestions and tool titles. It only
 * mounts while the drawer is open (AppKit's Drawer unmounts its body on
 * close), so the top bar never fetches the registry on pages where the
 * owner never opens Ask Ether.
 */
export default function AskEtherPanel({
  route,
  messages,
  labels,
  approvalRequestLabels,
  secretRequestLabels,
  send,
  onDecideApprovalRequest,
  onSubmitSecretRequest,
  hasOlderMessages,
  onLoadOlderMessages,
}: {
  route: string;
  messages: AgentMessage[];
  labels: Partial<AgentPanelLabels>;
  approvalRequestLabels: Partial<AgentApprovalRequestLabels>;
  secretRequestLabels: Partial<AgentSecretRequestLabels>;
  send: (prompt: string, signal: AbortSignal) => Promise<Response>;
  onDecideApprovalRequest: (
    approvalId: string,
    decision: "approved" | "rejected",
    note?: string,
  ) => Promise<void>;
  onSubmitSecretRequest: (requestId: string, value: string) => Promise<void>;
  hasOlderMessages: boolean;
  onLoadOlderMessages: () => Promise<void>;
}) {
  const panel = useRef<HTMLDivElement>(null);
  const [retryPrompt, setRetryPrompt] = useState("");
  const sendWithRetry = async (prompt: string, signal: AbortSignal) => {
    setRetryPrompt("");
    try {
      const response = await send(prompt, signal);
      if (!response.ok || !response.body) {
        setRetryPrompt(prompt);
        return response;
      }
      const decoder = new TextDecoder();
      let pending = "";
      const body = response.body.pipeThrough(
        new TransformStream<Uint8Array, Uint8Array>({
          transform(chunk, controller) {
            pending += decoder.decode(chunk, { stream: true });
            const lines = pending.split("\n");
            pending = lines.pop() ?? "";
            for (const line of lines) {
              if (!line.startsWith("data: {")) continue;
              try {
                // The SDK may stop reading on an error chunk, so expose retry
                // immediately instead of waiting for the stream to flush.
                if (JSON.parse(line.slice(6)).type === "error" && !signal.aborted)
                  setRetryPrompt(prompt);
              } catch {
                /* A non-JSON SSE line is not a turn error. */
              }
            }
            controller.enqueue(chunk);
          },
        }),
      );
      return new Response(body, { status: response.status, headers: response.headers });
    } catch (error) {
      if (!signal.aborted) setRetryPrompt(prompt);
      throw error;
    }
  };
  const { locale } = useLocale();
  const { descriptors } = useRegistry("command", "tool");

  const suggestions = useMemo(() => {
    if (route === "/livetv/channel-builder") return [];
    const fromRegistry = registrySuggestions(
      descriptors.filter((d) => d.kind === "command"),
      route,
    );
    const fallback = defaultSuggestion(route);
    const out = [...fromRegistry];
    if (!out.includes(fallback)) out.push(fallback);
    return out.slice(0, 4);
    // locale re-renders the fallback copy in the active language.
  }, [descriptors, route, locale]);

  const toolLabels = useMemo(() => {
    const humanized: Record<string, string> = {};
    for (const name of KNOWN_TOOLS) humanized[name] = t(humanizeToolName(name));
    return {
      ...humanized,
      livetv_channel_apply: t("Apply channel"),
      look_draft: t("Draft a style"),
      ...toolLabelsFromDescriptors(descriptors.filter((d) => d.kind === "tool")),
    };
  }, [descriptors, locale]);

  return (
    <div ref={panel} className="contents">
      <AgentPanel
        enabled
        initialMessages={messages}
        suggestions={suggestions}
        labels={labels}
        approvalRequestLabels={approvalRequestLabels}
        secretRequestLabels={secretRequestLabels}
        toolLabels={toolLabels}
        send={sendWithRetry}
        composerActions={
          retryPrompt ? (
            <Button
              variant="outline"
              size="sm"
              onClick={() => {
                const box = panel.current?.querySelector("textarea");
                if (!box) return;
                Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, "value")?.set?.call(
                  box,
                  retryPrompt,
                );
                box.dispatchEvent(new Event("input", { bubbles: true }));
                box.focus();
                requestAnimationFrame(() =>
                  box.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true })),
                );
              }}
            >
              {t("Try again")}
            </Button>
          ) : undefined
        }
        onDecideApprovalRequest={onDecideApprovalRequest}
        onSubmitSecretRequest={onSubmitSecretRequest}
        hasOlderMessages={hasOlderMessages}
        onLoadOlderMessages={onLoadOlderMessages}
      />
    </div>
  );
}
