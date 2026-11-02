/** Mock Ask Ether: turns, threads, approvals, secrets, MCP pairing (WS6). */
import type { ExtendHandler } from "./index";

export function createAgentMock(): { handle: ExtendHandler } {
  return { handle: () => null };
}
