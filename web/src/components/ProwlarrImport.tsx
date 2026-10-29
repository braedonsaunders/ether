import {
  Alert,
  AlertDescription,
  AlertTitle,
  Badge,
  Button,
  Input,
  SettingsSection,
  toast,
} from "@braedonsaunders/appkit-ui";
import { useState } from "react";
import { t, tn } from "../i18n";
import { apiFetch, errorMessage } from "../lib/api";
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
};

/** Preview and import the Usenet and torrent indexers a Prowlarr instance already manages. */
export function ProwlarrImport({ onImported }: { onImported?: () => void }) {
  const [url, setURL] = useState("");
  const [key, setKey] = useState("");
  const [result, setResult] = useState<Result | null>(null);
  const [busy, setBusy] = useState(false);
  async function run(commit: boolean) {
    setBusy(true);
    try {
      const r = await apiFetch<Result>("/playback/prowlarr", {
        method: "POST",
        body: JSON.stringify({
          config: { url, apiKey: key, dailyLimit: 0, dailyGrabs: 0, priority: 0 },
          import: commit,
        }),
      });
      setResult(r);
      if (commit) {
        setKey("");
        toast.success(t("Prowlarr indexers imported"));
        onImported?.();
      }
    } catch (e) {
      toast.error(errorMessage(e));
    } finally {
      setBusy(false);
    }
  }
  return (
    <div className="space-y-4">
      <p className="text-sm text-fg-muted">
        {t(
          "Bring in the Usenet (Newznab) and torrent (Torznab) indexers a Prowlarr instance manages. Ether handles their service limits automatically. Torrent indexers stream through a debrid account. You don't need Prowlarr: sites and Newznab/Torznab URLs can be added directly.",
        )}
      </p>
      <SettingsSection title="Prowlarr">
        <SettingsRow
          title={t("Prowlarr URL")}
          control={
            <Input
              aria-label={t("Prowlarr URL")}
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
          title={t("API key")}
          control={
            <Input
              aria-label={t("Prowlarr API key")}
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
          {t("Preview indexers")}
        </Button>
        <Button
          disabled={busy || !result || result.imported || !key}
          onClick={() => void run(true)}
        >
          {t("Import indexers")}
        </Button>
      </div>
      {result && (
        <ul className="space-y-2 text-sm">
          {result.indexers.map((i) => (
            <li key={i.id} className="flex flex-wrap items-center gap-2">
              <Badge variant="outline">{i.protocol === "torrent" ? "Torznab" : "Newznab"}</Badge>
              <span>{t("{name} · Automatic limits", { name: i.name })}</span>
            </li>
          ))}
        </ul>
      )}
      {result?.skippedUnknownProtocol ? (
        <p className="text-sm text-fg-muted">
          {tn(
            result.skippedUnknownProtocol,
            "Skipped {count} indexer with an unsupported protocol",
            "Skipped {count} indexers with an unsupported protocol",
          )}
        </p>
      ) : null}
      {result?.warnings?.length ? (
        <Alert variant="warning">
          <AlertTitle>{t("Worth knowing")}</AlertTitle>
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
