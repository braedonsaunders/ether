import { Badge } from "@braedonsaunders/appkit-ui";
import { localized, t, tx } from "../../i18n";
import type { Viewer, ViewerAccessStatus } from "../../lib/contract";

export const PLEX_ACCESS_URL = "https://app.plex.tv/desktop/#!/settings/manage-library-access";
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
  const status = viewer ? viewerStatus(viewer) : undefined;
  if (viewer && !viewer.plexId)
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
