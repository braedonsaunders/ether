/**
 * Mock Ask Ether: turns, threads, approvals, secrets, MCP pairing (WS6).
 *
 * Streams a real AI SDK UI message stream (SSE, one JSON chunk per event,
 * header x-vercel-ai-ui-message-stream: v1, terminated with data: [DONE])
 * so AppKit's AgentPanel decodes it exactly as it will the Go server.
 * Approval and secret parts only ever appear in the durable transcript
 * served by GET /agent/threads/{id}/messages, never in the live stream,
 * which is the contract AgentPanel is built around.
 */
import { referenceLooks } from "../../pages/livetv/onair/fixtures";
import { createStyleSteps } from "../create-style";
import type { Look } from "../../pages/livetv/onair/model";
import type { CollectionSpec, Rule } from "../../lib/contract";
import { normalizeSpec } from "../../lib/rules";
import { GENRES } from "../data";
import type { ExtendHandler } from "./index";

interface MockAgentMessage {
  id: string;
  role: "user" | "assistant" | "system";
  parts: unknown[];
  createdAt: string;
}

interface Thread {
  id: string;
  title: string;
  createdAt: string;
  updatedAt: string;
  messages: MockAgentMessage[];
}

interface ApprovalRecord {
  threadId: string;
  messageId: string;
  toolName: string;
}

interface SecretRecord {
  threadId: string;
  messageId: string;
  stored: boolean;
}

interface McpClient {
  id: string;
  name: string;
  createdAt: string;
  lastSeenAt?: string;
  scopes?: string[];
}

let mockAgentEnabled = true;
let mockAgentPaceMs = 0;

/** Demo control: milliseconds between streamed words, so a build can be watched. 0 in tests. */
export function setMockAgentPace(ms: number): void {
  mockAgentPaceMs = ms;
}

/** Test and demo control: pretend no language model is configured. */
export function setMockAgentEnabled(enabled: boolean): void {
  mockAgentEnabled = enabled;
}

function json(body: unknown, status = 200, headers: Record<string, string> = {}): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json", ...headers },
  });
}

function err(status: number, code: string, message: string): Response {
  return json({ error: { code, message } }, status);
}

let counter = 0;
function nextId(prefix: string): string {
  counter += 1;
  return `${prefix}-${counter}`;
}

function now(): string {
  return new Date().toISOString();
}

function titleFrom(prompt: string): string {
  const clean = prompt.trim().replace(/\s+/g, " ");
  return clean.length > 48 ? `${clean.slice(0, 47)}…` : clean || "New conversation";
}

interface PlannedTool {
  toolCallId: string;
  toolName: string;
  input: unknown;
  output: unknown;
}

interface PlannedTurn {
  text: string;
  tools: PlannedTool[];
  approval?: {
    approvalId: string;
    categoryLabel: string;
    description: string;
    details: { label: string; value: string }[];
  };
  secret?: {
    requestId: string;
    providerLabel: string;
    credentialLabel: string;
    purpose: string;
  };
}

function planTurn(prompt: string): PlannedTurn {
  const lower = prompt.toLowerCase();
  const tool = (toolName: string, input: unknown, output: unknown): PlannedTool => ({
    toolCallId: nextId("call"),
    toolName,
    input,
    output,
  });

  if (lower.includes("create") && lower.includes("channel")) {
    const approvalId = nextId("approval");
    return {
      text: "I've drafted a family movie channel with an evening schedule. Review it before I create it.",
      tools: [
        tool(
          "channel_draft",
          {},
          {
            channel: {
              id: "draft",
              name: "Family Movie Night",
              number: "14",
              enabled: true,
              timezone: "America/Toronto",
              kind: "virtual",
              favorite: false,
              epoch: new Date().toISOString(),
              seed: 1,
              shuffle: true,
              items: [],
            },
          },
        ),
      ],
      approval: {
        approvalId,
        categoryLabel: "Create a channel",
        description: "Create Family Movie Night with a family film schedule.",
        details: [
          { label: "Channel", value: "14 · Family Movie Night" },
          { label: "Schedule", value: "Family films every evening" },
        ],
      },
    };
  }

  if (lower.includes("blocklist")) {
    const approvalId = nextId("approval");
    return {
      text: "I found the release that was picked. Blocklisting it stops Ether from choosing it again, so the next play falls through to the next best release.",
      tools: [
        tool(
          "resolve_trace",
          { limit: 1 },
          { picked: "Example.Movie.2024.2160p.WEB-DL-GRP", candidates: 14 },
        ),
        tool(
          "release_blocklist",
          { release: "Example.Movie.2024.2160p.WEB-DL-GRP" },
          { status: "pending_approval", approvalId },
        ),
      ],
      approval: {
        approvalId,
        categoryLabel: "Blocklist a release",
        description: "Blocklist this release so Ether never picks it again.",
        details: [{ label: "Release", value: "Example.Movie.2024.2160p.WEB-DL-GRP" }],
      },
    };
  }

  if (
    lower.includes("debrid") ||
    lower.includes("api key") ||
    lower.includes("api token") ||
    lower.includes("credential") ||
    lower.includes("secret")
  ) {
    const requestId = nextId("secret");
    return {
      text: "I can save that credential for you. Enter it in the card below. It goes straight to sealed storage and is never shown here or sent to the model.",
      tools: [],
      secret: {
        requestId,
        providerLabel: "Real-Debrid",
        credentialLabel: "API token",
        purpose: "Ether uses this token to stream releases through your debrid account.",
      },
    };
  }

  if (lower.includes("go to") || lower.includes("open the") || lower.startsWith("show me")) {
    const route = lower.includes("live")
      ? "/livetv"
      : lower.includes("release")
        ? "/releases"
        : "/sources";
    return {
      text: "Opening that page for you.",
      tools: [tool("navigate", { route }, { navigate: route })],
    };
  }

  if (lower.includes("channel")) {
    return {
      text: "Two channels have no playable input right now: Example News and Example Sport. Their other inputs are healthy, so viewers still see the channels that recovered.",
      tools: [
        tool(
          "livetv_channels",
          { limit: 20 },
          {
            failing: [
              { name: "Example News", reason: "No playable input" },
              { name: "Example Sport", reason: "No playable input" },
            ],
          },
        ),
      ],
    };
  }

  if (lower.includes("stremio")) {
    return {
      text: "A Stremio plugin is added from Sources with its manifest URL. Ether reads the manifest, shows what the plugin offers, and treats the install URL as a secret.",
      tools: [
        tool(
          "sources_list",
          { limit: 20 },
          { sources: [{ name: "Example indexer", health: "ok" }] },
        ),
      ],
    };
  }

  if (lower.includes("release") || lower.includes("why did") || lower.includes("pick")) {
    return {
      text: "This title picked that release because it ranked first on resolution and connection health, and it was already cached. The next candidate was smaller but unverified, so the ladder put it second.",
      tools: [
        tool(
          "resolve_trace",
          { limit: 1 },
          { picked: "Example.Movie.2024.2160p.WEB-DL-GRP", reason: "Cached and verified" },
        ),
      ],
    };
  }

  if (lower.includes("attention")) {
    return {
      text: "Nothing needs urgent attention. One Live TV channel lost an input overnight and recovered on its backup, and everything else reports healthy.",
      tools: [
        tool("server_status", {}, { overall: "ok", attention: ["One channel input recovered"] }),
      ],
    };
  }

  return {
    text: "Your server reports healthy. Ask me why a title picked a release, which channels are failing, or ask me to blocklist a release.",
    tools: [tool("server_status", {}, { overall: "ok" })],
  };
}

function streamChunks(messageId: string, plan: PlannedTurn): string[] {
  const chunks: unknown[] = [{ type: "start", messageId }, { type: "start-step" }];
  for (const t of plan.tools) {
    chunks.push(
      { type: "tool-input-start", toolCallId: t.toolCallId, toolName: t.toolName, dynamic: true },
      {
        type: "tool-input-available",
        toolCallId: t.toolCallId,
        toolName: t.toolName,
        input: t.input,
        dynamic: true,
      },
      {
        type: "tool-output-available",
        toolCallId: t.toolCallId,
        output: t.output,
        dynamic: true,
      },
    );
  }
  const textId = nextId("text");
  chunks.push({ type: "text-start", id: textId });
  for (const word of plan.text.split(/(?<=\s)/)) {
    if (word) chunks.push({ type: "text-delta", id: textId, delta: word });
  }
  chunks.push(
    { type: "text-end", id: textId },
    { type: "finish-step" },
    { type: "finish", finishReason: "stop" },
  );
  return chunks.map((chunk) => `data: ${JSON.stringify(chunk)}\n\n`);
}

/** "Describe a collection": the drafts a model would try for these asks. */
function describeSpec(prompts: string[], mediaType: string): CollectionSpec {
  const ask = prompts.join(" ").toLowerCase();
  const genres = GENRES.filter(
    (g) => ask.includes(g.toLowerCase()) || (g === "Science Fiction" && /sci-?fi/.test(ask)),
  );
  const rules: Rule[] = genres.length ? [{ field: "genre", op: "in", value: genres }] : [];
  const years = /(\d{4})\s*(?:to|-|–|and|through)\s*(\d{4})/.exec(ask);
  const decade = /(19|20)(\d)0s/.exec(ask) ?? /\b(\d)0s\b/.exec(ask);
  if (years)
    rules.push({ field: "year", op: "between", value: [Number(years[1]), Number(years[2])] });
  else if (decade)
    rules.push({
      field: "decade",
      op: "in",
      value: [decade[0].length === 5 ? decade[0].slice(0, 4) : `19${decade[1]}0`],
    });
  if (/short|quick|under/.test(ask)) rules.push({ field: "runtime", op: "lte", value: 95 });
  const ranked = /acclaimed|best|great|top/.test(ask);
  if (/acclaimed|great reviews/.test(ask)) rules.push({ field: "rating", op: "gte", value: 7.5 });
  if (ranked) rules.push({ field: "votes", op: "gte", value: 500 });
  const top = /top (\d+)/.exec(ask);
  const first = prompts[0] ?? "";
  return normalizeSpec({
    name: first.replace(/^./, (c) => c.toUpperCase()).slice(0, 48),
    description: `Built from “${first}”.`,
    mediaType:
      mediaType === "show" || mediaType === "mixed"
        ? mediaType
        : /tv|show|series/.test(ask)
          ? "show"
          : "movie",
    rules,
    sort: ranked ? "rating" : "popularity",
    sortDesc: true,
    limit: top ? Number(top[1]) : 50,
    enabled: true,
  });
}

interface DescribeStep {
  text: string;
  draft?: PlannedTool;
}

function planDescribe(prompts: string[], mediaType: string): DescribeStep[] {
  const spec = describeSpec(prompts, mediaType);
  const draft = (s: CollectionSpec): PlannedTool => ({
    toolCallId: nextId("call"),
    toolName: "collection_draft",
    input: { spec: s, mediaType: s.mediaType },
    output: { spec: s, total: s.limit, titles: [] },
  });
  const steps: DescribeStep[] = [];
  if (spec.sort === "rating" && prompts.length === 1) {
    const loose = { ...spec, rules: spec.rules.filter((r) => r.field !== "votes") };
    steps.push({
      text: "I'll start from your whole catalogue and keep only what matches. ",
      draft: draft(loose),
    });
    steps.push({
      text: "A few little-known titles crowd the top, so I'll only count ones with plenty of votes. ",
      draft: draft(spec),
    });
  } else {
    steps.push({ text: "Updating the draft with that change. ", draft: draft(spec) });
  }
  steps.push({
    text: `“${spec.name}” holds ${spec.limit} titles that match, ranked by ${spec.sort === "rating" ? "rating" : "popularity"}. You could ask for only one genre, or a shorter runtime.`,
  });
  return steps;
}

/** Chunks and the pause before each, for a describe turn. */
function describeChunks(
  messageId: string,
  steps: DescribeStep[],
): { lines: string[]; delays: number[] } {
  const lines: string[] = [];
  const delays: number[] = [];
  const push = (chunk: unknown, delay = 0) => {
    lines.push(`data: ${JSON.stringify(chunk)}\n\n`);
    delays.push(delay);
  };
  push({ type: "start", messageId });
  for (const step of steps) {
    push({ type: "start-step" });
    const textId = nextId("text");
    push({ type: "text-start", id: textId });
    for (const word of step.text.split(/(?<=\s)/))
      if (word) push({ type: "text-delta", id: textId, delta: word }, mockAgentPaceMs);
    push({ type: "text-end", id: textId });
    if (step.draft) {
      const d = step.draft;
      push({
        type: "tool-input-start",
        toolCallId: d.toolCallId,
        toolName: d.toolName,
        dynamic: true,
      });
      push({
        type: "tool-input-available",
        toolCallId: d.toolCallId,
        toolName: d.toolName,
        input: d.input,
        dynamic: true,
      });
      push(
        {
          type: "tool-output-available",
          toolCallId: d.toolCallId,
          output: d.output,
          dynamic: true,
        },
        mockAgentPaceMs * 15,
      );
    }
    push({ type: "finish-step" });
  }
  push({ type: "finish", finishReason: "stop" });
  return { lines, delays };
}

function describeParts(steps: DescribeStep[]): unknown[] {
  const parts: unknown[] = [];
  for (const step of steps) {
    parts.push({ type: "text", text: step.text, state: "done" });
    if (step.draft)
      parts.push({
        type: "dynamic-tool",
        toolName: step.draft.toolName,
        toolCallId: step.draft.toolCallId,
        state: "output-available",
        input: step.draft.input,
        output: step.draft.output,
      });
  }
  return parts;
}

function sseResponse(lines: string[], threadId: string, delays: number[] = []): Response {
  const encoder = new TextEncoder();
  let index = 0;
  const body = new ReadableStream<Uint8Array>({
    async pull(controller) {
      const wait = delays[index] ?? 0;
      if (wait > 0) await new Promise((resolve) => setTimeout(resolve, wait));
      if (index < lines.length) {
        controller.enqueue(encoder.encode(lines[index]!));
        index += 1;
        return;
      }
      if (index === lines.length) {
        controller.enqueue(encoder.encode("data: [DONE]\n\n"));
        index += 1;
        return;
      }
      controller.close();
    },
  });
  return new Response(body, {
    status: 200,
    headers: {
      "content-type": "text/event-stream",
      "cache-control": "no-store",
      "x-vercel-ai-ui-message-stream": "v1",
      "x-ether-thread-id": threadId,
    },
  });
}

function asRecord(body: unknown): Record<string, unknown> {
  return body && typeof body === "object" ? (body as Record<string, unknown>) : {};
}

function storedParts(plan: PlannedTurn): unknown[] {
  const parts: unknown[] = plan.tools.map((t) => ({
    type: "dynamic-tool",
    toolName: t.toolName,
    toolCallId: t.toolCallId,
    state: "output-available",
    input: t.input,
    output: t.output,
  }));
  parts.push({ type: "text", text: plan.text, state: "done" });
  if (plan.approval) {
    parts.push({ type: "approval-request", status: "pending", ...plan.approval });
  }
  if (plan.secret) {
    parts.push({ type: "secret-request", status: "pending", ...plan.secret });
  }
  return parts;
}

function findPart(
  threads: Map<string, Thread>,
  threadId: string,
  messageId: string,
  type: string,
  idKey: string,
  id: string,
): Record<string, unknown> | null {
  const message = threads.get(threadId)?.messages.find((m) => m.id === messageId);
  if (!message) return null;
  for (const part of message.parts) {
    const record = part as Record<string, unknown>;
    if (record.type === type && record[idKey] === id) return record;
  }
  return null;
}

export function createAgentMock(): { handle: ExtendHandler } {
  const threads = new Map<string, Thread>();
  const approvals = new Map<string, ApprovalRecord>();
  const secrets = new Map<string, SecretRecord>();
  let clients: McpClient[] = [
    {
      id: "mcp-client-1",
      name: "Sample AI app",
      createdAt: "2026-10-20T12:00:00.000Z",
      lastSeenAt: "2026-10-31T09:30:00.000Z",
      scopes: ["read"],
    },
  ];

  const handle: ExtendHandler = (method, seg, q, body) => {
    if (seg[0] !== "agent") return null;
    const input = asRecord(body);

    if (method === "GET" && seg[1] === "status") {
      return json(
        mockAgentEnabled
          ? { enabled: true, disabled: false, status: "ready", model: "Mock model", tools: true }
          : { enabled: false, disabled: true, status: "disabled", tools: false },
      );
    }

    if (method === "GET" && seg[1] === "threads" && !seg[2]) {
      const list = [...threads.values()]
        .sort((a, b) => b.updatedAt.localeCompare(a.updatedAt))
        .map((thread) => ({
          id: thread.id,
          title: thread.title,
          createdAt: thread.createdAt,
          updatedAt: thread.updatedAt,
          messageCount: thread.messages.length,
        }));
      return json({ threads: list });
    }

    if (method === "GET" && seg[1] === "threads" && seg[2] && seg[3] === "messages") {
      const thread = threads.get(seg[2]);
      if (!thread) return err(404, "not_found", "Conversation not found.");
      const before = q.get("before");
      if (before) return json({ threadId: thread.id, messages: [], hasMore: false });
      return json({ threadId: thread.id, messages: thread.messages, hasMore: false });
    }

    if (method === "DELETE" && seg[1] === "threads" && seg[2] && !seg[3]) {
      if (!threads.delete(seg[2])) return err(404, "not_found", "Conversation not found.");
      return json({ ok: true });
    }

    if (method === "POST" && seg[1] === "turn" && !seg[2]) {
      if (!mockAgentEnabled)
        return err(409, "model_not_configured", "No language model is configured.");
      const prompt = typeof input.prompt === "string" ? input.prompt : "";
      if (!prompt.trim()) return err(400, "invalid", "A prompt is required.");
      let thread = typeof input.threadId === "string" ? threads.get(input.threadId) : undefined;
      if (!thread) {
        thread = {
          id: nextId("thread"),
          title: titleFrom(prompt),
          createdAt: now(),
          updatedAt: now(),
          messages: [],
        };
        threads.set(thread.id, thread);
      }
      thread.messages.push({
        id: nextId("msg"),
        role: "user",
        parts: [{ type: "text", text: prompt }],
        createdAt: now(),
      });
      const context = asRecord(input.context);
      if (context.route === "/collections/describe") {
        const prompts = thread.messages.flatMap((m) =>
          m.role === "user" ? m.parts.map((p) => String((p as { text?: string }).text ?? "")) : [],
        );
        const steps = planDescribe(prompts, String(context.mediaType ?? ""));
        const id = nextId("msg");
        thread.messages.push({
          id,
          role: "assistant",
          parts: describeParts(steps),
          createdAt: now(),
        });
        thread.updatedAt = now();
        const { lines, delays } = describeChunks(id, steps);
        return sseResponse(lines, thread.id, delays);
      }
      if (context.route === "/livetv/look") {
        const draft = asRecord(context.draft);
        const steps = createStyleSteps(
          draft.look as Look,
          prompt,
          draft.sceneId as string | undefined,
        );
        const id = nextId("msg");
        thread.messages.push({
          id,
          role: "assistant",
          parts: describeParts(steps),
          createdAt: now(),
        });
        const { lines, delays } = describeChunks(id, steps);
        return sseResponse(lines, thread.id, delays);
      }
      const plan = planTurn(
        context.route === "/livetv/channel-builder" ? "create a channel" : prompt,
      );
      if (context.route === "/livetv/channel-builder") {
        const draft = asRecord(asRecord(context.draft).channel);
        const output = asRecord(plan.tools[0]?.output);
        output.channel = {
          ...draft,
          name: "Family Movie Night",
          filter: { type: "movie", genres: ["Adventure"], yearFrom: 1980, yearTo: 1989 },
          items: [],
        };
        if (!/no (theme|branding|look)/i.test(prompt)) {
          const base = structuredClone(referenceLooks[0]!);
          base.name = "Family Movie Night";
          const steps = createStyleSteps(base, prompt);
          const last = [...steps].reverse().find((step) => "draft" in step);
          if (last && "draft" in last) output.style = last.draft.output.look;
        }
        if (plan.tools[0]) {
          plan.tools[0].toolName = "livetv_channel_apply";
          plan.tools[0].output = output;
        }
        if (plan.approval) {
          plan.approval.details = [
            { label: "Channel", value: `${draft.number} · Family Movie Night` },
            { label: "Schedule", value: "Family films every evening" },
          ];
        }
        plan.text =
          "I've drafted the programming. Review it below; Apply updates your editor draft.";
      }
      const assistant: MockAgentMessage = {
        id: nextId("msg"),
        role: "assistant",
        parts: storedParts(plan),
        createdAt: now(),
      };
      thread.messages.push(assistant);
      thread.updatedAt = now();
      if (plan.approval) {
        approvals.set(plan.approval.approvalId, {
          threadId: thread.id,
          messageId: assistant.id,
          toolName: plan.tools.at(-1)?.toolName ?? "release_blocklist",
        });
      }
      if (plan.secret) {
        secrets.set(plan.secret.requestId, {
          threadId: thread.id,
          messageId: assistant.id,
          stored: false,
        });
      }
      return sseResponse(streamChunks(assistant.id, plan), thread.id);
    }

    if (
      method === "POST" &&
      (seg[1] === "turn" || seg[1] === "turns") &&
      seg[2] &&
      seg[3] === "cancel"
    ) {
      return json({ ok: true });
    }

    if (method === "POST" && seg[1] === "approvals" && seg[2]) {
      const record = approvals.get(seg[2]);
      if (!record) return err(404, "not_found", "Approval not found.");
      const decision = input.decision === "approved" ? "approved" : "rejected";
      const part = findPart(
        threads,
        record.threadId,
        record.messageId,
        "approval-request",
        "approvalId",
        seg[2],
      );
      if (part) {
        part.status = decision;
        if (typeof input.note === "string" && input.note) part.decisionNote = input.note;
      }
      const thread = threads.get(record.threadId);
      if (thread) {
        thread.messages.push({
          id: nextId("msg"),
          role: "assistant",
          parts: [
            {
              type: "text",
              text:
                record.toolName === "channel_draft" || record.toolName === "livetv_channel_apply"
                  ? decision === "approved"
                    ? "Channel draft approved."
                    : "Channel draft declined."
                  : decision === "approved"
                    ? "Done. That release is blocklisted, so the next play uses the next best release."
                    : "Okay, nothing changed. That release stays available.",
              state: "done",
            },
          ],
          createdAt: now(),
        });
        thread.updatedAt = now();
      }
      const proposal = thread?.messages
        .find((m) => m.id === record.messageId)
        ?.parts.find((p) => asRecord(p).type === "dynamic-tool");
      return json({
        status: decision,
        result: decision === "approved" ? asRecord(proposal).output : undefined,
      });
    }

    if (method === "POST" && seg[1] === "secrets" && seg[2]) {
      const record = secrets.get(seg[2]);
      if (!record) return err(404, "not_found", "Credential request not found.");
      // The value is read once to confirm it exists and is never stored,
      // logged, echoed or written into the transcript.
      if (typeof input.value !== "string" || input.value.length === 0)
        return err(400, "invalid", "A value is required.");
      record.stored = true;
      const part = findPart(
        threads,
        record.threadId,
        record.messageId,
        "secret-request",
        "requestId",
        seg[2],
      );
      if (part) part.status = "stored";
      const thread = threads.get(record.threadId);
      if (thread) thread.updatedAt = now();
      return json({ status: "stored" });
    }

    if (seg[1] === "mcp" && seg[2] === "authorization") {
      if (method === "GET") return json({ name: "Claude", scope: "read" });
      if (method === "POST") return json({ redirect: "#mcp-complete" });
    }
    if (method === "GET" && seg[1] === "mcp" && seg[2] === "clients" && !seg[3]) {
      return json({ clients });
    }

    if (method === "DELETE" && seg[1] === "mcp" && seg[2] === "clients" && seg[3]) {
      const before = clients.length;
      clients = clients.filter((client) => client.id !== seg[3]);
      if (clients.length === before) return err(404, "not_found", "Paired app not found.");
      return json({ ok: true });
    }

    return null;
  };

  return { handle };
}
