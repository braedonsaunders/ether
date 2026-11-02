import { Badge } from "@braedonsaunders/appkit-ui";
import { useAppOptional } from "../../app-context";
import { localized, t, tx } from "../../i18n";
import type { Viewer, ViewerAccessStatus, ViewerLifecycle } from "../../lib/contract";

export const PLEX_ACCESS_URL = "https://app.plex.tv/desktop/#!/settings/manage-library-access";

/**
 * The viewer's lifecycle. Servers send it on every viewer; the fallback
 * derives it from the owner flag and Plex link so partial payloads (and the
 * mock's DLNA guests) still behave safely: Plex viewers are never managed
 * in Ether.
 */
export function viewerLifecycle(viewer: Viewer): ViewerLifecycle {
  if (viewer.lifecycle)
    // Plex viewers are managed on plex.tv and are never shown as paused.
    return viewer.lifecycle.managedBy === "plex" && viewer.lifecycle.paused
      ? { ...viewer.lifecycle, paused: false }
      : viewer.lifecycle;
  if (viewer.isOwner)
    return {
      source: "owner",
      paused: false,
      plexShared: false,
      canRemove: false,
      canMerge: false,
      canPause: false,
      managedBy: "",
    };
  if (viewer.plexId > 0)
    return {
      source: "plex",
      paused: false,
      plexShared: viewerStatus(viewer) === "active",
      canRemove: false,
      canMerge: false,
      canPause: false,
      managedBy: "plex",
    };
  return {
    source: "local",
    paused: false,
    plexShared: false,
    canRemove: true,
    canMerge: true,
    canPause: true,
    managedBy: "",
  };
}
export function viewerStatus(viewer: Viewer): ViewerAccessStatus {
  return viewer.accessStatus ?? (viewer.isOwner ? "owner" : "active");
}
const LABELS: Record<ViewerAccessStatus, string> = localized(() => ({
  owner: t("Owner"),
  active: t("Has access"),
  imported: t("Imported history"),
  inactive: t("No current access"),
}));
export function ViewerStatus({ viewer }: { viewer: Viewer }) {
  const status = viewerStatus(viewer);
  return <Badge variant={status === "active" ? "success" : "default"}>{LABELS[status]}</Badge>;
}
export function PlexAccessNote({ viewer }: { viewer?: Viewer }) {
  const plexOn = useAppOptional()?.boot.plexEnabled !== false;
  const status = viewer ? viewerStatus(viewer) : undefined;
  if (!plexOn || (viewer && !viewer.plexId))
    return <p className="text-sm text-fg-muted">{t("Access is managed by the owner.")}</p>;
  return (
    <p className="text-sm text-fg-muted">
      {status === "imported"
        ? `${t("Imported history only; this viewer does not currently have server access.")} `
        : status === "inactive"
          ? `${t("This viewer no longer has server access. Their history is retained.")} `
          : ""}
      {tx("Plex sharing is managed on {link}.", {
        link: (
          <a
            href={PLEX_ACCESS_URL}
            target="_blank"
            rel="noopener noreferrer"
            className="font-medium text-primary hover:underline"
          >
            Plex.tv
          </a>
        ),
      })}
    </p>
  );
}
