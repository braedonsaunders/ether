import {
  Alert,
  AlertDescription,
  AlertTitle,
  Badge,
  Button,
  Input,
  MultiSelect,
  SettingsSection,
  toast,
} from "@braedonsaunders/appkit-ui";
import { useEffect, useState } from "react";
import { t, tn } from "../i18n";
import { apiFetch, errorMessage } from "../lib/api";
import { PasswordInput } from "./fields";
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
  /** Enabled source indexers skipped for an unsupported protocol. Import is additive when no tags are chosen. */
  skippedUnknownProtocol?: number;
  warnings?: string[];
  /** "ok" after a read, "unreachable" when Prowlarr could not be read. */
  status?: string;
  /** True when a failed sync left the previous indexers in place. */
  kept?: boolean;
  /** Indexers disabled because they no longer have a chosen tag. */
  stopped?: number;
};

type Tag = { id: number; label: string };

type Saved = {
  config?: { url?: string; apiKey?: string; tags?: number[] };
  status?: string;
  kept?: boolean;
};

/** Preview and import the Usenet and torrent indexers a Prowlarr instance already manages. */
export function ProwlarrImport({ onImported }: { onImported?: () => void }) {
  const [url, setURL] = useState("");
  const [key, setKey] = useState("");
  const [tags, setTags] = useState<string[]>([]);
  const [tagOptions, setTagOptions] = useState<Tag[]>([]);
  const [tagsLoaded, setTagsLoaded] = useState(false);
  const [result, setResult] = useState<Result | null>(null);
  const [status, setStatus] = useState("");
  const [kept, setKept] = useState(false);
  const [busy, setBusy] = useState(false);

  const options = tagOptions.map((tag) => ({ value: String(tag.id), label: tag.label }));
  for (const id of tags) {
    if (!options.some((o) => o.value === id))
      options.push({ value: id, label: t("Tag {id}", { id }) });
  }

  async function loadTags(nextURL = url, nextKey = key) {
    const r = await apiFetch<{ tags: Tag[] }>("/playback/prowlarr/tags", {
      method: "POST",
      body: JSON.stringify({ config: { url: nextURL, apiKey: nextKey } }),
    });
    setTagOptions(r.tags ?? []);
    setTagsLoaded(true);
  }

  useEffect(() => {
    let live = true;
    apiFetch<Saved>("/playback/prowlarr")
      .then(async (saved) => {
        if (!live) return;
        const nextURL = saved.config?.url ?? "";
        const nextKey = saved.config?.apiKey ?? "";
        setURL((prev) => prev || nextURL);
        setKey((prev) => prev || nextKey);
        setTags((prev) => (prev.length ? prev : (saved.config?.tags ?? []).map(String)));
        setStatus((prev) => prev || saved.status || "");
        setKept((prev) => prev || Boolean(saved.kept));
        if (nextURL && nextKey) {
          try {
            const r = await apiFetch<{ tags: Tag[] }>("/playback/prowlarr/tags", {
              method: "POST",
              body: JSON.stringify({ config: { url: nextURL, apiKey: nextKey } }),
            });
            if (!live) return;
            setTagOptions(r.tags ?? []);
            setTagsLoaded(true);
          } catch {
            // The picker still shows the saved tag ids.
          }
        }
      })
      .catch(() => {});
    return () => {
      live = false;
    };
  }, []);

  function configBody() {
    const config: {
      url: string;
      apiKey: string;
      dailyLimit: number;
      dailyGrabs: number;
      priority: number;
      tags?: number[];
    } = { url, apiKey: key, dailyLimit: 0, dailyGrabs: 0, priority: 0 };
    if (tags.length) config.tags = tags.map(Number);
    return config;
  }

  async function run(commit: boolean) {
    setBusy(true);
    try {
      const r = await apiFetch<Result>("/playback/prowlarr", {
        method: "POST",
        body: JSON.stringify({ config: configBody(), import: commit }),
      });
      setResult(r);
      setStatus(r.status ?? "");
      setKept(Boolean(r.kept));
      if (r.status === "unreachable") return;
      if (commit) {
        toast.success(t("Prowlarr indexers imported"));
        onImported?.();
      }
    } catch (e) {
      setStatus("unreachable");
      setKept(false);
      toast.error(errorMessage(e));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="space-y-4">
      {status === "unreachable" ? (
        <Alert variant="warning">
          <AlertTitle>{t("Prowlarr is unreachable")}</AlertTitle>
          {kept ? <AlertDescription>{t("Still using the last indexers.")}</AlertDescription> : null}
        </Alert>
      ) : null}
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
            <PasswordInput
              aria-label={t("Prowlarr API key")}
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
        <SettingsRow
          title={t("Tags")}
          description={t("Leave empty to use every indexer.")}
          control={
            <div className="space-y-2 sm:w-72">
              <MultiSelect
                value={tags}
                onChange={(next) => {
                  setTags(next);
                  setResult(null);
                }}
                options={options}
                ariaLabel={t("Indexer tags")}
                placeholder={tagOptions.length ? t("Every indexer") : t("Load tags")}
                searchPlaceholder={t("Search…")}
                noMatchesLabel={t("No matches")}
                clearLabel={t("Clear")}
                selectAllLabel={t("Select all")}
                clearable
                maxVisibleValues={4}
              />
              <Button
                variant="outline"
                size="sm"
                disabled={busy || !url || !key}
                onClick={() =>
                  void loadTags().catch((e) => {
                    toast.error(errorMessage(e));
                  })
                }
              >
                {t("Load tags")}
              </Button>
              {tagsLoaded && tagOptions.length === 0 ? (
                <p className="text-xs text-fg-muted">{t("No tags on this Prowlarr.")}</p>
              ) : null}
            </div>
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
      {result?.stopped ? (
        <p className="text-sm text-fg-muted">
          {tn(
            result.stopped,
            "{count} indexer no longer has a chosen tag",
            "{count} indexers no longer have a chosen tag",
          )}
        </p>
      ) : null}
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
