import type { EtherConfig } from "../../lib/types";
import { appPath, appRank } from "../apps/catalog";

const CONNECT_PANEL_APPS = ["jellyfin", "emby", "silo", "dlna"];

/** The finish chip opens the app that shows a server address, not the Apps overview. */
export function connectDevicePath(frontends: EtherConfig["frontends"]): string {
  const enabled = Object.entries(frontends ?? {})
    .filter(([, row]) => row?.enabled)
    .map(([id]) => id)
    .sort((a, b) => appRank(a) - appRank(b));
  const panel = enabled.find((id) => CONNECT_PANEL_APPS.includes(id));
  const id = panel ?? enabled[0];
  if (!id) return "/apps";
  if (["stremio", "webdav", "m3u"].includes(id)) return "/me";
  return `/apps/${appPath(id)}?connect=1`;
}
