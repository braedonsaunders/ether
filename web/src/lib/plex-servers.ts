import { apiFetch } from "./api";

interface PMSMapping {
  accountId: number;
  accountName: string;
  owner: boolean;
  userId: number;
  direction: number;
  interval: number;
  canSend: boolean;
  lastImport: number;
  lastSend: number;
  nextSync: number;
  problem: string;
  unmatched: number;
}

export interface PMSServer {
  id: number;
  name: string;
  url: string;
  machineId: string;
  version: string;
  owner: string;
  checkedAt: number;
  problem: string;
  viewers: PMSMapping[];
}

/** The single registry of owner-run Plex servers, shared by sync and one-time import. */
export const listPlexServers = (signal?: AbortSignal) =>
  apiFetch<PMSServer[]>("/plex-servers", { signal });
