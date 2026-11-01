import { Button } from "@braedonsaunders/appkit-ui";
import { Pencil } from "lucide-react";
import type { ReactNode } from "react";
import { DragHandle, type DragHandleProps } from "../../components/Sortable";
import { Drawer } from "../../components/Drawer";
import { t } from "../../i18n";
import { usenetProblem } from "../../lib/health";
import type { DebridKind } from "../../lib/types";
import { StremioMark, debridMark } from "./marks";

/** Where each service shows the token Ether asks for. */
export const DEBRID_TOKEN_URL: Record<DebridKind, string> = {
  realdebrid: "https://real-debrid.com/apitoken",
  torbox: "https://torbox.app/settings",
  alldebrid: "https://alldebrid.com/apikeys",
  premiumize: "https://www.premiumize.me/account",
  debridlink: "https://debrid-link.com/webapp/apikey",
  offcloud: "https://offcloud.com/#/account",
};

/** Services Ether can stream from. */
export const DEBRID_KINDS: { value: DebridKind; label: string }[] = [
  { value: "realdebrid", label: "Real-Debrid" },
  { value: "torbox", label: "TorBox" },
  { value: "alldebrid", label: "AllDebrid" },
  { value: "premiumize", label: "Premiumize" },
  { value: "debridlink", label: "Debrid-Link" },
  { value: "offcloud", label: "Offcloud" },
];

/**
 * Fallback letters when no vendored mark exists.
 * Real logos are local Simple Icons copies (see assets/brands/PROVENANCE.md).
 * Never hotlink, and never draw a logo that Simple Icons does not publish.
 */
export function sourceMonogram(name: string): string {
  const letters = name
    .trim()
    .split(/[\s-]+/)
    .slice(0, 2)
    .map((part) => part[0] ?? "")
    .join("")
    .toUpperCase();
  return letters || "?";
}

export function SourceMark({ name, logo }: { name: string; logo?: ReactNode }) {
  return (
    <span
      aria-hidden
      className="flex size-11 shrink-0 items-center justify-center overflow-hidden rounded-lg bg-bg-subtle text-sm font-semibold text-fg-muted"
    >
      {logo ?? sourceMonogram(name)}
    </span>
  );
}

/** Plain-language reason a debrid account will not be used, or null when it is fine. */
export function debridFix(enabled: boolean, state: string | undefined): string | null {
  if (!enabled) return t("This account is off, so Ether skips it. Turn it on to use it.");
  switch (state) {
    case "auth_failed":
      return t("The API key was rejected. Open this account and paste a new key from the service.");
    case "backoff":
      return t(
        "The service asked Ether to wait. Other accounts are tried first, and this one retries on its own.",
      );
    default:
      return null;
  }
}

/** Plain-language reason a Usenet provider will not be used, or null when it is fine. */
export function usenetFix(enabled: boolean, state: unknown, lastError?: unknown): string | null {
  if (!enabled) return t("This provider is off, so Ether skips it. Turn it on to use it.");
  return usenetProblem(state, lastError);
}

/** One configured source: order, health and the next thing to do, edited from the card. */
export function SourceCard({
  name,
  position,
  description,
  meta,
  problem,
  editLabel,
  onEdit,
  order,
  handle,
  logo,
}: {
  name: string;
  /** 1-based place in the try order. */
  position: number;
  description?: ReactNode;
  meta?: ReactNode;
  problem?: string | null;
  editLabel: string;
  onEdit: () => void;
  order?: ReactNode;
  handle?: DragHandleProps;
  /** Vendored brand mark. Omitted brands keep the monogram. */
  logo?: ReactNode;
}) {
  return (
    <article className="flex items-start gap-3 rounded-xl border border-border bg-surface p-4">
      {handle ? <DragHandle handle={handle} label={t("Drag to reorder {name}", { name })} /> : null}
      <SourceMark name={name} logo={logo} />
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-center gap-2">
          <span className="text-xs font-medium tabular-nums text-fg-subtle">{position}</span>
          <h3 className="font-semibold text-fg">{name}</h3>
          {meta}
        </div>
        {description ? <div className="mt-1 text-sm text-fg-muted">{description}</div> : null}
        {problem ? (
          <p className="mt-2 text-sm text-danger" role="status">
            {problem}
          </p>
        ) : null}
      </div>
      <div className="flex shrink-0 items-center gap-1">
        {order}
        <Button variant="outline" size="sm" onClick={onEdit} aria-label={editLabel}>
          <Pencil size={13} /> {t("Edit")}
        </Button>
      </div>
    </article>
  );
}

export type GalleryChoice =
  | { type: "debrid"; kind: DebridKind }
  | { type: "usenet" }
  | { type: "site" }
  | { type: "newznab" }
  | { type: "stremio" }
  | { type: "prowlarr" };

function debridBlurb(kind: DebridKind): string {
  switch (kind) {
    case "realdebrid":
      return t("Stream torrents Real-Debrid already has.");
    case "torbox":
      return t("Stream torrents TorBox already has.");
    case "alldebrid":
      return t("Stream torrents AllDebrid already has.");
    case "premiumize":
      return t("Stream torrents Premiumize already has.");
    case "debridlink":
      return t("Stream torrents Debrid-Link already has.");
    case "offcloud":
      return t("Stream torrents Offcloud already has.");
    default:
      return t("Stream torrents this service already has.");
  }
}

function GalleryButton({
  name,
  description,
  onClick,
  logo,
}: {
  name: string;
  description: string;
  onClick: () => void;
  logo?: ReactNode;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      className="flex h-full flex-col rounded-xl border border-border bg-surface p-5 text-left transition-colors hover:bg-surface-hover focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50"
    >
      <span className="flex items-start gap-3">
        <SourceMark name={name} logo={logo} />
        <span className="font-semibold text-fg">{name}</span>
      </span>
      <span className="mt-3 text-sm text-fg-muted">{description}</span>
    </button>
  );
}

function GalleryGroup({
  title,
  hint,
  children,
}: {
  title: string;
  hint: string;
  children: ReactNode;
}) {
  return (
    <section aria-label={title} className="space-y-3">
      <div>
        <h3 className="text-sm font-semibold text-fg">{title}</h3>
        <p className="text-sm text-fg-muted">{hint}</p>
      </div>
      <div className="grid gap-3 sm:grid-cols-2">{children}</div>
    </section>
  );
}

/** Pick what to add. Choosing one opens that record's drawer. */
export function AddSourceGallery({
  open,
  onClose,
  onPick,
}: {
  open: boolean;
  onClose: () => void;
  onPick: (choice: GalleryChoice) => void;
}) {
  return (
    <Drawer
      open={open}
      onClose={onClose}
      title={t("Add a source")}
      description={t("An account to stream from, or a place that searches for releases.")}
      size="lg"
    >
      <div className="space-y-6">
        <GalleryGroup
          title={t("Debrid services")}
          hint={t(
            "These stream torrent releases. You also need a torrent indexer, site or Stremio addon.",
          )}
        >
          {DEBRID_KINDS.map((kind) => (
            <GalleryButton
              key={kind.value}
              name={kind.label}
              description={debridBlurb(kind.value)}
              logo={debridMark(kind.value) ?? undefined}
              onClick={() => onPick({ type: "debrid", kind: kind.value })}
            />
          ))}
        </GalleryGroup>
        <GalleryGroup
          title={t("Usenet")}
          hint={t(
            "An NNTP account. Block accounts can be marked as backfill, used only when an article is missing elsewhere.",
          )}
        >
          <GalleryButton
            name={t("Usenet provider")}
            description={t(
              "Host, username and password from your Usenet plan. Ether tries providers in order.",
            )}
            onClick={() => onPick({ type: "usenet" })}
          />
        </GalleryGroup>
        <GalleryGroup
          title={t("Where to search")}
          hint={t(
            "Ether searches these when someone presses Play. Limits from the service are followed automatically.",
          )}
        >
          <GalleryButton
            name={t("Tracker site")}
            description={t(
              "A site Ether already knows how to sign in to. Its settings come from the site's definition.",
            )}
            onClick={() => onPick({ type: "site" })}
          />
          <GalleryButton
            name={t("Newznab or Torznab")}
            description={t(
              "A search address and API key. Torznab results need a debrid account to play.",
            )}
            onClick={() => onPick({ type: "newznab" })}
          />
          <GalleryButton
            name={t("Stremio addon")}
            description={t(
              "An addon install link that returns torrents. Ether streams them through your debrid accounts.",
            )}
            logo={<StremioMark />}
            onClick={() => onPick({ type: "stremio" })}
          />
          <GalleryButton
            name={t("Prowlarr")}
            description={t("Copy the indexers a Prowlarr instance already manages.")}
            onClick={() => onPick({ type: "prowlarr" })}
          />
        </GalleryGroup>
      </div>
    </Drawer>
  );
}
