import {
  Alert,
  AlertDescription,
  AlertTitle,
  Badge,
  Button,
  Input,
  SettingsSection,
  confirmDialog,
  toast,
} from "@braedonsaunders/appkit-ui";
import { useState } from "react";
import { ApiError, apiFetch, errorMessage } from "../lib/api";
import { SettingsRow } from "./SettingsRow";

type Result = {
  indexers: {
    id: string;
    name: string;
    dailyLimit: number;
    dailyGrabs: number;
    /** "torrent" for Torznab indexers; empty or "usenet" for Newznab. */
    protocol?: string;
  }[];
  imported: boolean;
  /** Enabled source indexers skipped for an unsupported protocol. Import is additive: nothing is ever removed. */
  skippedUnknownProtocol?: number;
  warnings?: string[];
  /** True when this result came from an explicit sync request. */
  sync?: boolean;
  /** Same-scope indexers a sync preview would remove, or did remove. Only sent for sync requests. */
  removals?: { id: string; name: string }[];
  /** Count actually removed by a sync apply (0 on a sync preview). */
  removed?: number;
  /**
   * Same-scope indexers kept because they were edited locally: identity
   * (ID suffix, URL, name marker) no longer matches the generated pattern,
   * or a present entry differs from the generated one (name, URL, limits,
   * grabs, priority, enabled, protocol, categories — API key excluded).
   */
  keptEdited?: { id: string; name: string }[];
  /** Removal IDs a sync preview approved; the apply must echo them back. */
  approvedRemovals?: string[];
  /** Deterministic fingerprint of the sync preview; the apply must echo it back. */
  plan?: string;
};

/** Preview and import the Usenet and torrent indexers a Prowlarr instance already manages. */
export function ProwlarrImport({ onImported }: { onImported?: () => void }) {
  const [url, setURL] = useState("");
  const [key, setKey] = useState("");
  const [result, setResult] = useState<Result | null>(null);
  const [busy, setBusy] = useState(false);
  async function run(
    commit: boolean,
    sync = false,
    syncPlan?: { plan: string; approvedRemovals: string[] },
    rethrowConflict = false,
  ): Promise<Result | null> {
    setBusy(true);
    try {
      const r = await apiFetch<Result>("/playback/prowlarr", {
        method: "POST",
        body: JSON.stringify({
          config: { url, apiKey: key, dailyLimit: 0, dailyGrabs: 0, priority: 0 },
          import: commit,
          // Only an explicit sync sends sync=true; the plain preview and
          // import requests keep their original shape and stay additive.
          // A sync apply is bound to its preview: it echoes the approved
          // removal IDs and the plan fingerprint back to the server.
          ...(sync
            ? {
                sync: true,
                ...(commit && syncPlan
                  ? { approvedRemovals: syncPlan.approvedRemovals, plan: syncPlan.plan }
                  : {}),
              }
            : {}),
        }),
      });
      const stored = sync ? { ...r, sync: true } : r;
      setResult(stored);
      if (commit) {
        setKey("");
        toast.success(sync ? "Prowlarr indexers synced" : "Prowlarr indexers imported");
        onImported?.();
      }
      return stored;
    } catch (e) {
      if (rethrowConflict && e instanceof ApiError && e.status === 409) throw e;
      toast.error(errorMessage(e));
      return null;
    } finally {
      setBusy(false);
    }
  }

  /**
   * Explicit reconciliation: fetch a sync preview first (nothing is
   * removed by it), show exactly what would be removed, and only apply
   * after the owner confirms. Cancelling applies nothing.
   */
  async function sync() {
    const preview = await run(false, true);
    if (!preview) return;
    const removals = preview.removals ?? [];
    const kept = preview.keptEdited ?? [];
    const ok = await confirmDialog({
      title: "Sync from Prowlarr?",
      message:
        removals.length > 0
          ? `This will remove ${removals.length} previously imported indexer${removals.length === 1 ? "" : "s"} that Prowlarr no longer reports as enabled: ${removals.map((i) => i.name || i.id).join(", ")}.${kept.length > 0 ? ` ${kept.length} locally edited indexer${kept.length === 1 ? "" : "s"} will be kept.` : ""}`
          : `No previously imported indexers will be removed.${kept.length > 0 ? ` ${kept.length} locally edited indexer${kept.length === 1 ? "" : "s"} will be kept.` : ""} Discovered indexers will be added or updated.`,
      confirmLabel: "Sync",
      tone: removals.length > 0 ? "danger" : "default",
    });
    if (!ok) return;
    try {
      await run(
        true,
        true,
        {
          plan: preview.plan ?? "",
          approvedRemovals: preview.approvedRemovals ?? removals.map((i) => i.id),
        },
        true,
      );
    } catch {
      // 409: the source (or the local indexers) changed after the
      // preview, so the approved plan is stale and nothing was applied.
      // Say so and fetch a fresh preview for a new confirmation.
      toast.error("Source changed, review again");
      await run(false, true);
    }
  }
  return (
    <div className="space-y-4">
      <p className="text-sm text-fg-muted">
        Bring in the Usenet (Newznab) and torrent (Torznab) indexers a Prowlarr instance manages.
        Ether handles their service limits automatically. Torrent indexers stream through a debrid
        account. You don't need Prowlarr: built-in sites and Newznab/Torznab URLs can be added
        directly.
      </p>
      <SettingsSection title="Prowlarr">
        <SettingsRow
          title="Prowlarr URL"
          control={
            <Input
              aria-label="Prowlarr URL"
              type="url"
              className="sm:w-72"
              placeholder="http://localhost:9696"
              value={url}
              onChange={(e) => {
                setURL(e.target.value);
                setResult(null);
              }}
            />
          }
        />
        <SettingsRow
          title="API key"
          control={
            <Input
              aria-label="Prowlarr API key"
              type="password"
              autoComplete="off"
              className="sm:w-72"
              value={key}
              onChange={(e) => {
                setKey(e.target.value);
                setResult(null);
              }}
            />
          }
        />
      </SettingsSection>
      <div className="flex flex-wrap gap-2">
        <Button variant="outline" disabled={busy || !url || !key} onClick={() => void run(false)}>
          Preview indexers
        </Button>
        <Button
          disabled={busy || !result || result.imported || !key}
          onClick={() => void run(true)}
        >
          Import indexers
        </Button>
        <Button variant="outline" disabled={busy || !url || !key} onClick={() => void sync()}>
          Sync from Prowlarr
        </Button>
      </div>
      <p className="text-sm text-fg-muted">
        Sync is separate from import: it first previews which previously imported indexers would be
        removed and only applies after you confirm, bound to that preview. Indexers you edited
        locally (name, URL or limits) are kept and never overwritten by a sync.
      </p>
      {result && (
        <ul className="space-y-2 text-sm">
          {result.indexers.map((i) => (
            <li key={i.id} className="flex flex-wrap items-center gap-2">
              <Badge variant="outline">{i.protocol === "torrent" ? "Torznab" : "Newznab"}</Badge>
              <span>{i.name} · Automatic limits</span>
            </li>
          ))}
        </ul>
      )}
      {result?.sync && result.removals && result.removals.length > 0 ? (
        <div className="space-y-1 text-sm">
          <p className="font-medium">
            {result.imported
              ? `Removed ${result.removed ?? result.removals.length} previously imported indexer${(result.removed ?? result.removals.length) === 1 ? "" : "s"}`
              : `Would remove ${result.removals.length} previously imported indexer${result.removals.length === 1 ? "" : "s"}`}
          </p>
          <ul className="list-disc space-y-1 pl-5">
            {result.removals.map((i) => (
              <li key={i.id}>{i.name || i.id}</li>
            ))}
          </ul>
        </div>
      ) : null}
      {result?.sync && result.imported && result.removals?.length === 0 ? (
        <p className="text-sm text-fg-muted">Removed 0 previously imported indexers</p>
      ) : null}
      {result?.sync && result.keptEdited && result.keptEdited.length > 0 ? (
        <div className="space-y-1 text-sm">
          <p className="font-medium">Kept locally edited indexers</p>
          <ul className="list-disc space-y-1 pl-5">
            {result.keptEdited.map((i) => (
              <li key={i.id}>{i.name || i.id}</li>
            ))}
          </ul>
        </div>
      ) : null}
      {result?.skippedUnknownProtocol ? (
        <p className="text-sm text-fg-muted">
          {`Skipped ${result.skippedUnknownProtocol} indexer${result.skippedUnknownProtocol === 1 ? "" : "s"} with an unsupported protocol`}
        </p>
      ) : null}
      {result?.warnings?.length ? (
        <Alert variant="warning">
          <AlertTitle>Worth knowing</AlertTitle>
          <AlertDescription>
            <ul className="mt-1 list-disc space-y-1 pl-5">
              {result.warnings.map((w) => (
                <li key={w}>{w}</li>
              ))}
            </ul>
          </AlertDescription>
        </Alert>
      ) : null}
    </div>
  );
}
