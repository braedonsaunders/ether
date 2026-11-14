/**
 * Ask Ether wire types and pure helpers (SPEC §5.11). The shapes here are
 * the WS4/WS5 API contract; parsing accepts a wrapped object or a bare
 * array where the spec leaves the envelope open.
 */
import type { AgentMessage, AgentPanelLabels } from "@braedonsaunders/appkit-ai/react";
import { t } from "../../i18n";
import { apiFetch } from "../../lib/api";
import { textOf, type Descriptor } from "../../lib/extend";

export type { AgentMessage };

export interface AgentStatus {
  enabled: boolean;
  model?: string;
}

export interface AgentThreadSummary {
  id: string;
  title: string;
  createdAt?: string;
  updatedAt?: string;
}

export interface McpClient {
  id: string;
  name: string;
  createdAt?: string;
  lastSeenAt?: string;
  scopes?: string[];
}

type Record_ = Record<string, unknown>;

function record(value: unknown): Record_ {
  return value && typeof value === "object" ? (value as Record_) : {};
}

function listOf<T>(value: unknown, key: string): T[] {
  if (Array.isArray(value)) return value as T[];
  const inner = record(value)[key];
  return Array.isArray(inner) ? (inner as T[]) : [];
}

/** Disabled is the only status that changes the drawer; accept its spellings. */
export function parseAgentStatus(value: unknown): AgentStatus {
  const body = record(value);
  const disabled =
    body.enabled === false ||
    body.disabled === true ||
    body.configured === false ||
    body.status === "disabled";
  return {
    enabled: !disabled,
    model: typeof body.model === "string" ? body.model : undefined,
  };
}

export async function fetchAgentStatus(): Promise<AgentStatus> {
  return parseAgentStatus(await apiFetch<unknown>("/agent/status"));
}

export function parseThreads(value: unknown): AgentThreadSummary[] {
  return listOf<Record_>(value, "threads").map((thread) => ({
    id: String(thread.id ?? ""),
    title: typeof thread.title === "string" && thread.title ? thread.title : t("Conversation"),
    createdAt: typeof thread.createdAt === "string" ? thread.createdAt : undefined,
    updatedAt: typeof thread.updatedAt === "string" ? thread.updatedAt : undefined,
  }));
}

export async function fetchAgentThreads(): Promise<AgentThreadSummary[]> {
  return parseThreads(await apiFetch<unknown>("/agent/threads"));
}

export interface AgentMessagesPage {
  messages: AgentMessage[];
  hasOlder: boolean;
  /** A turn is still running in this conversation (a reload rejoins it). */
  running: boolean;
}

export function parseMessages(value: unknown): AgentMessagesPage {
  const body = record(value);
  return {
    messages: listOf<AgentMessage>(value, "messages"),
    hasOlder: body.hasMore === true || body.hasOlder === true || body.hasOlderMessages === true,
    running: body.running === true,
  };
}

export async function fetchAgentMessages(
  threadId: string,
  before?: string,
): Promise<AgentMessagesPage> {
  const query = before ? `?before=${encodeURIComponent(before)}` : "";
  return parseMessages(
    await apiFetch<unknown>(`/agent/threads/${encodeURIComponent(threadId)}/messages${query}`),
  );
}

export async function deleteAgentThread(threadId: string): Promise<void> {
  await apiFetch<unknown>(`/agent/threads/${encodeURIComponent(threadId)}`, { method: "DELETE" });
}

/** Stops a running turn by its assistant message id. */
export async function cancelAgentTurn(turnId: string): Promise<void> {
  await apiFetch<unknown>(`/agent/turns/${encodeURIComponent(turnId)}/cancel`, { method: "POST" });
}

export async function decideAgentApproval(
  approvalId: string,
  decision: "approved" | "rejected",
  note?: string,
  context?: { route: string; record?: { type: string; id: string } },
): Promise<{
  status: string;
  failureReason?: string;
  result?: { channel?: import("../../pages/livetv/model").Channel };
}> {
  // The server runs the agent's follow-up turn before it replies.
  return apiFetch(`/agent/approvals/${encodeURIComponent(approvalId)}`, {
    method: "POST",
    body: JSON.stringify({ decision, note, context }),
  });
}

export async function submitAgentSecret(requestId: string, value: string): Promise<void> {
  await apiFetch<unknown>(`/agent/secrets/${encodeURIComponent(requestId)}`, {
    method: "POST",
    body: JSON.stringify({ value }),
  });
}

export function parseMcpClients(value: unknown): McpClient[] {
  return listOf<Record_>(value, "clients").map((client) => ({
    id: String(client.id ?? ""),
    name: typeof client.name === "string" && client.name ? client.name : t("AI app"),
    createdAt: typeof client.createdAt === "string" ? client.createdAt : undefined,
    lastSeenAt: typeof client.lastSeenAt === "string" ? client.lastSeenAt : undefined,
    scopes: Array.isArray(client.scopes) ? client.scopes.map(String) : undefined,
  }));
}

export async function fetchMcpClients(): Promise<McpClient[]> {
  return parseMcpClients(await apiFetch<unknown>("/agent/mcp/clients"));
}

export async function revokeMcpClient(id: string): Promise<void> {
  await apiFetch<unknown>(`/agent/mcp/clients/${encodeURIComponent(id)}`, { method: "DELETE" });
}

/** The record open under the current route, when the route names one. */
export function inferRecord(pathname: string): { type: string; id: string } | undefined {
  const seg = pathname.split("/").filter(Boolean);
  if (seg.length < 2) return undefined;
  const [head, id] = seg as [string, string];
  if (head === "viewers" && /^[1-9]\d*$/.test(id)) return { type: "viewer", id };
  if (head === "collections" && id !== "new") return { type: "collection", id };
  return undefined;
}

/** One browse pattern: exact, a trailing wildcard, or a path prefix. */
export function matchRoutePattern(pattern: string, route: string): boolean {
  const clean = pattern.trim();
  if (!clean) return false;
  if (clean === "*") return true;
  if (clean.includes("*")) {
    const source = clean
      .split("*")
      .map((part) => part.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"))
      .join(".*");
    return new RegExp(`^${source}$`).test(route);
  }
  if (clean === "/") return route === "/";
  return route === clean || route.startsWith(`${clean.replace(/\/$/, "")}/`);
}

export interface AgentSuggestionMeta {
  agentSuggestion?: boolean;
  routePatterns?: string[];
}

/** Registry command prompts flagged for Ask Ether on this route. */
export function registrySuggestions(descriptors: Descriptor[], route: string): string[] {
  const out: string[] = [];
  for (const descriptor of descriptors) {
    const meta = record(descriptor.meta) as AgentSuggestionMeta;
    if (!meta.agentSuggestion) continue;
    const patterns = meta.routePatterns ?? [];
    if (patterns.length && !patterns.some((pattern) => matchRoutePattern(pattern, route))) continue;
    const text = textOf(descriptor.label);
    if (text && !out.includes(text)) out.push(text);
  }
  return out;
}

/** Sentence-case a snake_case tool name for its card when no title is served. */
export function humanizeToolName(name: string): string {
  const words = name.replace(/__/g, " ").replace(/_/g, " ").trim();
  return words ? words.charAt(0).toUpperCase() + words.slice(1) : name;
}

/** Tool card titles from served tool descriptors, keyed by tool name. */
export function toolLabelsFromDescriptors(descriptors: Descriptor[]): Record<string, string> {
  const labels: Record<string, string> = {};
  for (const descriptor of descriptors) {
    const title = textOf(descriptor.label);
    if (!title) continue;
    const meta = record(descriptor.meta);
    const names = [descriptor.id, typeof meta.name === "string" ? meta.name : ""];
    for (const name of names) {
      if (!name) continue;
      labels[name] = title;
      // Descriptor ids may be namespaced (tool/<name> or plugin ids with dots).
      const tail = name.split("/").pop() ?? name;
      labels[tail] = title;
      labels[tail.replace(/\./g, "_")] = title;
    }
  }
  return labels;
}

function navigateFrom(value: unknown): string | null {
  const body = record(value);
  for (const key of ["navigate", "route", "path"]) {
    const candidate = body[key];
    if (typeof candidate === "string" && candidate.startsWith("/")) return candidate;
  }
  return navigateFromRecord(body.data) ?? null;
}

function navigateFromRecord(value: unknown): string | null {
  if (!value || typeof value !== "object") return null;
  const body = value as Record_;
  for (const key of ["navigate", "route", "path"]) {
    const candidate = body[key];
    if (typeof candidate === "string" && candidate.startsWith("/")) return candidate;
  }
  return null;
}

/** The first navigate route carried by a tool result in a transcript. */
export function extractNavigateRoute(
  messages: AgentMessage[],
  seen: ReadonlySet<string>,
): { route: string; key: string } | null {
  for (const message of messages) {
    if (message.role !== "assistant") continue;
    for (const raw of message.parts) {
      const part = record(raw);
      const type = typeof part.type === "string" ? part.type : "";
      const isTool = type === "dynamic-tool" || type.startsWith("tool-");
      if (!isTool) continue;
      const output = part.output;
      if (typeof output === "string") continue;
      const route = navigateFrom(output);
      if (!route) continue;
      const callId = typeof part.toolCallId === "string" ? part.toolCallId : "";
      const key = `${message.id}:${callId}:${route}`;
      if (!seen.has(key)) return { route, key };
    }
  }
  return null;
}

/** The console's AgentPanel copy; pages override the few lines that differ. */
export function agentPanelLabels(): AgentPanelLabels {
  return {
    title: t("Ask Ether"),
    welcomeTitle: t("How can Ether help?"),
    welcomeDescription: t(
      "Ask about this server, or ask Ether to make a change. Changes always wait for your approval.",
    ),
    disabledTitle: t("Connect a model"),
    disabledDescription: t("Connect a language model to use Ask Ether."),
    placeholder: t("Ask Ether…"),
    send: t("Send"),
    stop: t("Stop"),
    failed: t("Ether could not finish that. Try again."),
    input: t("Input"),
    result: t("Result"),
    working: t("Working"),
    step: t("step"),
    steps: t("steps"),
    queue: t("Add to queue"),
    queueTitle: t("Up next"),
    queuePosition: t("Position"),
    queued: t("Queued"),
    dispatching: t("Starting"),
    retrying: t("Retrying"),
    queueFailed: t("This queued message needs attention."),
    editQueued: t("Edit queued message"),
    removeQueued: t("Remove queued message"),
    retryQueued: t("Retry queued message"),
    sendQueuedNow: t("Send now"),
    responding: t("Ether is responding"),
    loadEarlier: t("Load earlier messages"),
    loadingEarlier: t("Loading earlier messages"),
    loadEarlierFailed: t("Earlier messages could not be loaded. Try again."),
  };
}
