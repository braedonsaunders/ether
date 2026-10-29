import { t } from "../../i18n";
import type { EtherUser } from "../../lib/types";

/** One aggregate bucket from /stats/summary (a day, an hour, a device, a title, a viewer). */
export type Bucket = { label: string; plays: number; watchedMs: number; bytes: number };

export type Summary = {
  plays: number;
  watchedMs: number;
  bytes: number;
  completed: number;
  daily: Bucket[];
  hourly: Bucket[];
  platforms: Bucket[];
  titles: Bucket[];
  users: Bucket[];
};

export type Play = {
  id: string;
  userId: number;
  username: string;
  title: string;
  device: string;
  platform: string;
  decision: string;
  started: number;
  ended: number;
  bytes: number;
  watchedMs: number;
  completed: boolean;
  failovers: number;
  error: string;
};

/** Period, viewer and year filters shared by History and Stats. */
export interface ViewingFilter {
  days: string;
  user: string;
  year: string;
}

export const viewerLabel = (viewer: EtherUser) => {
  const name = viewer.title || viewer.username;
  return viewer.isOwner ? t("{name} (owner)", { name }) : name;
};

export function viewingQuery(f: ViewingFilter): string {
  return `?days=${f.days}&user=${encodeURIComponent(f.user)}${f.year ? `&year=${f.year}` : ""}`;
}
