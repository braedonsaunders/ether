/** Mock console stream: GET /api/stream server-sent events (WS2). */
import type { ExtendHandler } from "./index";

export function createStreamMock(): { handle: ExtendHandler } {
  return { handle: () => null };
}
