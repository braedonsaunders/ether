/**
 * Mock backend for the extensibility work (docs/extend/SPEC.md): the
 * descriptor registry, plugins, Ask Ether and the console stream. Each area
 * lives in its own file owned by one workstream; this file only routes.
 * Handlers return a Response (so streams can stream) or null to fall through.
 */
import type { Descriptor } from "../../lib/extend";
import { descriptors as ranking } from "./descriptors/ranking";
import { descriptors as collections } from "./descriptors/collections";
import { descriptors as livetv } from "./descriptors/livetv";
import { descriptors as notify } from "./descriptors/notify";
import { descriptors as consoleDescriptors } from "./descriptors/console";
import { descriptors as settings } from "./descriptors/settings";
import { descriptors as subtitles } from "./descriptors/subtitles";
import { descriptors as sources } from "./descriptors/sources";
import { createPluginsMock } from "./plugins";
import { createAgentMock } from "./agent";
import { createStreamMock } from "./stream";

export type ExtendHandler = (
  method: string,
  seg: string[],
  q: URLSearchParams,
  body: unknown,
) => Response | Promise<Response> | null;

export function builtinDescriptors(): Descriptor[] {
  return [
    ...ranking,
    ...collections,
    ...livetv,
    ...notify,
    ...consoleDescriptors,
    ...settings,
    ...subtitles,
    ...sources,
  ];
}

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

export function createExtendMock(): ExtendHandler {
  const plugins = createPluginsMock();
  const agent = createAgentMock();
  const stream = createStreamMock();
  return (method, seg, q, body) => {
    if (method === "GET" && seg[0] === "extend" && seg[1] === "registry") {
      const kinds = (q.get("kinds") ?? "").split(",").filter(Boolean);
      const all = [...builtinDescriptors(), ...plugins.descriptors()];
      const descriptors = kinds.length ? all.filter((d) => kinds.includes(d.kind)) : all;
      return json({ version: plugins.version(), descriptors });
    }
    return (
      plugins.handle(method, seg, q, body) ??
      agent.handle(method, seg, q, body) ??
      stream.handle(method, seg, q, body)
    );
  };
}
