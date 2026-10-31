import {
  Alert,
  AlertDescription,
  Badge,
  Button,
  EmptyState,
  Input,
  SearchSelect,
  SettingsSection,
  Textarea,
  confirmDialog,
  toast,
} from "@braedonsaunders/appkit-ui";
import { Download, Plus, RefreshCw, Upload } from "lucide-react";
import { useEffect, useMemo, useState } from "react";
import { t, tn } from "../../i18n";
import { searchSelectLabels } from "../../i18n/appkit-labels";
import { Drawer } from "../../components/Drawer";
import { ErrorPanel, LoadingRows, MutationError } from "../../components/feedback";
import { ChipGroup, SecretInput } from "../../components/fields";
import { SaveBar, useDraft } from "../../components/SaveBar";
import { SettingsRow } from "../../components/SettingsRow";
import { api, ApiError, newId } from "../../lib/api";
import { formatDate, formatNumber } from "../../lib/format";
import type {
  CatalogEntry,
  EtherConfig,
  Indexer,
  IndexerSitesConfig,
  SiteDefinition,
  SiteSetting,
} from "../../lib/types";
import { BoolRow, TextRow, recordErrors, toastError } from "./form";

/** Sites: indexers Ether searches itself from an installed definition. */
export function isBuiltin(ix: { kind?: string }) {
  return ix.kind === "cardigann";
}

export function isInfoSetting(s: SiteSetting) {
  return s.type.startsWith("info");
}

function settingDefault(s: SiteSetting): string {
  switch (s.type) {
    case "checkbox":
      return ["true", "1", "yes", "on"].includes((s.default ?? "").toLowerCase())
        ? "true"
        : "false";
    case "multi-select":
      return (s.defaults?.length ? s.defaults : s.default ? [s.default] : []).join(",");
    case "select":
      return s.default ?? s.options?.[0]?.value ?? "";
    default:
      return s.default ?? "";
  }
}

/** A new indexer for a definition, with the definition's defaults filled in. */
export function blankSiteIndexer(def: SiteDefinition, priority: number): Indexer {
  const settings: Record<string, string> = {};
  const secretSettings: Record<string, string> = {};
  for (const s of def.settings) {
    if (isInfoSetting(s)) continue;
    if (s.type === "password") secretSettings[s.name] = "";
    else settings[s.name] = settingDefault(s);
  }
  return {
    id: newId("site"),
    name: def.name,
    url: def.links[0] ?? "",
    apiKey: "",
    dailyLimit: 0,
    dailyGrabs: 0,
    priority,
    enabled: true,
    movieCategories: null,
    tvCategories: null,
    protocol: def.protocol === "torrent" ? "torrent" : "",
    kind: "cardigann",
    definition: def.id,
    settings,
    secretSettings,
  };
}

/** "Torrent" or "Usenet", as shown on definition badges. */
export function siteProtocolLabel(protocol?: string) {
  return protocol === "usenet" || protocol === "" ? "Usenet" : "Torrent";
}

/** "Torrent site" or "Usenet site": a site indexer's type, as shown on its badge. */
export function siteKindLabel(protocol?: string) {
  return protocol === "usenet" || protocol === "" ? t("Usenet site") : t("Torrent site");
}

function definitionOrigin(def: SiteDefinition) {
  return def.source === "source" ? t("From your definitions source") : t("Uploaded");
}

function plainText(html: string) {
  return html
    .replace(/<br\s*\/?>/gi, " ")
    .replace(/<[^>]+>/g, "")
    .replace(/\s+/g, " ")
    .trim();
}

function DefinitionBadges({ def, protocol = true }: { def: SiteDefinition; protocol?: boolean }) {
  return (
    <>
      {protocol ? <Badge variant="outline">{siteProtocolLabel(def.protocol)}</Badge> : null}
      {def.type ? <Badge variant="secondary">{def.type}</Badge> : null}
      {def.language ? <Badge variant="secondary">{def.language}</Badge> : null}
    </>
  );
}

function DefinitionWarnings({ def }: { def: SiteDefinition }) {
  if (!def.warnings?.length) return null;
  return <p className="text-xs text-warning">{def.warnings.join(" · ")}</p>;
}

/**
 * The definition behind a site indexer, in its drawer: the one place an
 * in-use definition is shown, with where it came from and any warnings.
 */
export function SiteDefinitionRow({ def }: { def: SiteDefinition }) {
  return (
    <SettingsRow
      title={t("Definition")}
      description={
        <>
          <span className="block">
            {def.name} ({def.id})
          </span>
          <span className="block">
            {def.source === "source"
              ? t("From your definitions source, updated {date}", {
                  date: formatDate(def.updatedAt),
                })
              : t("Uploaded {date}", { date: formatDate(def.updatedAt) })}
          </span>
          <DefinitionWarnings def={def} />
        </>
      }
      control={
        <span className="flex flex-wrap gap-2">
          <DefinitionBadges def={def} protocol={false} />
        </span>
      }
    />
  );
}

/**
 * The definition's own settings (account, options, guidance) for one
 * indexer. Password settings go to `secretSettings`, which the server seals
 * and redacts like an API key.
 */
export function SiteSettingsFields({
  def,
  draft,
  onChange,
  errors,
}: {
  def: SiteDefinition;
  draft: Indexer;
  onChange: (patch: Partial<Indexer>) => void;
  errors: Record<string, string>;
}) {
  const settings = draft.settings ?? {};
  const secrets = draft.secretSettings ?? {};
  const setSetting = (name: string, v: string) =>
    onChange({ settings: { ...settings, [name]: v } });
  const setSecret = (name: string, v: string) =>
    onChange({ secretSettings: { ...secrets, [name]: v } });
  const fields = def.settings.filter((s) => !isInfoSetting(s) || s.label || s.default);
  if (!fields.length) return null;
  return (
    <SettingsSection
      title={t("Site settings")}
      description={t("Settings {site} needs.", { site: def.name })}
    >
      {fields.map((s) => {
        const label = s.label || s.name;
        const value = settings[s.name] ?? settingDefault(s);
        if (isInfoSetting(s))
          return (
            <SettingsRow
              key={s.name}
              stacked
              title={label}
              description={s.default ? plainText(s.default) : undefined}
            />
          );
        switch (s.type) {
          case "password":
            return (
              <SettingsRow key={s.name} stacked title={label}>
                <SecretInput
                  ariaLabel={label}
                  value={secrets[s.name] ?? ""}
                  onChange={(v) => setSecret(s.name, v)}
                  invalid={Boolean(errors[`secretSettings.${s.name}`])}
                />
              </SettingsRow>
            );
          case "checkbox":
            return (
              <BoolRow
                key={s.name}
                label={label}
                value={value === "true"}
                onChange={(v) => setSetting(s.name, v ? "true" : "false")}
              />
            );
          case "select":
            return (
              <SettingsRow
                key={s.name}
                title={label}
                control={
                  <div className="w-56">
                    <SearchSelect
                      value={value}
                      onChange={(v) => setSetting(s.name, v)}
                      options={(s.options ?? []).map((o) => ({ value: o.value, label: o.label }))}
                      ariaLabel={label}
                      searchable={(s.options ?? []).length > 7}
                      {...searchSelectLabels()}
                    />
                  </div>
                }
              />
            );
          case "multi-select":
            return (
              <SettingsRow key={s.name} stacked title={label}>
                <ChipGroup
                  ariaLabel={label}
                  size="sm"
                  options={(s.options ?? []).map((o) => ({ value: o.value, label: o.label }))}
                  value={value ? value.split(",") : []}
                  onChange={(v) => setSetting(s.name, v.join(","))}
                />
              </SettingsRow>
            );
          default:
            return (
              <TextRow
                key={s.name}
                label={label}
                value={value}
                error={errors[`settings.${s.name}`]}
                onChange={(v) => setSetting(s.name, v)}
              />
            );
        }
      })}
    </SettingsSection>
  );
}

/**
 * Pick the site for a new site indexer: an installed definition, one from the
 * owner's definitions source (installed on pick), or an uploaded file.
 */
export function SitePicker({
  definitions,
  onPick,
  onInstalled,
  onUpload,
  inUse,
}: {
  definitions: SiteDefinition[];
  /** Definition ids configured indexers use. */
  inUse: Set<string>;
  onPick: (def: SiteDefinition) => void;
  onInstalled: () => void;
  /** Upload a definition file, then add it. */
  onUpload: () => void;
}) {
  const [query, setQuery] = useState("");
  const [catalog, setCatalog] = useState<CatalogEntry[] | null>(null);
  const [catalogState, setCatalogState] = useState<"idle" | "loading" | "none" | "error">("idle");
  const [catalogError, setCatalogError] = useState<unknown>(null);
  const [installing, setInstalling] = useState<string | null>(null);

  async function loadCatalog(refresh = false) {
    setCatalogState("loading");
    try {
      setCatalog(await api.sites.catalog(refresh));
      setCatalogState("idle");
    } catch (e) {
      if (e instanceof ApiError && e.code === "no_source") setCatalogState("none");
      else {
        setCatalogError(e);
        setCatalogState("error");
      }
    }
  }
  useEffect(() => {
    void loadCatalog();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const q = query.trim().toLowerCase();
  const installed = definitions.filter(
    (d) =>
      !q ||
      d.name.toLowerCase().includes(q) ||
      d.id.toLowerCase().includes(q) ||
      (d.description ?? "").toLowerCase().includes(q),
  );
  const installedIds = new Set(definitions.map((d) => d.id));
  const offered = (catalog ?? []).filter(
    (e) => !e.installed && !installedIds.has(e.id) && (!q || e.id.toLowerCase().includes(q)),
  );

  async function install(id: string) {
    setInstalling(id);
    try {
      const def = await api.sites.install(id);
      toast.success(t("{name} installed", { name: def.name }));
      onInstalled();
      onPick(def);
    } catch (e) {
      toastError(t("Couldn't install {name}", { name: id }), e);
    } finally {
      setInstalling(null);
    }
  }

  return (
    <div className="space-y-4">
      <div className="flex gap-2">
        <Input
          aria-label={t("Search sites")}
          placeholder={t("Search sites")}
          value={query}
          onChange={(e) => setQuery(e.currentTarget.value)}
        />
        <Button variant="outline" onClick={onUpload} className="shrink-0">
          <Upload size={14} /> {t("Upload definition")}
        </Button>
      </div>
      <section aria-label={t("Installed sites")} className="space-y-2">
        <h3 className="text-sm font-semibold text-fg">{t("Installed")}</h3>
        {definitions.length === 0 ? (
          <p className="text-sm text-fg-muted">
            {t(
              "No site definitions installed yet. Upload one, or install one from your definitions source below.",
            )}
          </p>
        ) : installed.length === 0 ? (
          <p className="text-sm text-fg-muted">{t("No installed site matches.")}</p>
        ) : (
          <ul className="divide-y divide-border rounded-xl border border-border">
            {installed.map((d) => (
              <li key={d.id} className="flex flex-wrap items-center gap-3 px-4 py-2.5">
                <div className="min-w-0 flex-1 basis-48">
                  <p className="text-sm font-medium text-fg">{d.name}</p>
                  {d.description ? (
                    <p className="truncate text-xs text-fg-muted">{d.description}</p>
                  ) : null}
                </div>
                {inUse.has(d.id) ? <Badge variant="success">{t("Added")}</Badge> : null}
                <DefinitionBadges def={d} />
                <Button
                  size="sm"
                  variant="outline"
                  onClick={() => onPick(d)}
                  aria-label={t("Use {name}", { name: d.name })}
                >
                  {t("Use")}
                </Button>
              </li>
            ))}
          </ul>
        )}
      </section>
      <section aria-label={t("From your definitions source")} className="space-y-2">
        <div className="flex items-center justify-between gap-2">
          <h3 className="text-sm font-semibold text-fg">{t("From your definitions source")}</h3>
          {catalogState !== "none" ? (
            <Button
              size="sm"
              variant="ghost"
              onClick={() => void loadCatalog(true)}
              disabled={catalogState === "loading"}
            >
              <RefreshCw size={13} /> {t("Refresh")}
            </Button>
          ) : null}
        </div>
        {catalogState === "none" ? (
          <p className="text-sm text-fg-muted">
            {t(
              "No definitions source is set. Add one under Site definitions, below your indexers, to browse it here.",
            )}
          </p>
        ) : catalogState === "error" ? (
          <MutationError error={catalogError} />
        ) : catalogState === "loading" ? (
          <LoadingRows rows={2} />
        ) : offered.length === 0 ? (
          <p className="text-sm text-fg-muted">
            {q
              ? t("Nothing in the source matches.")
              : t("Everything the source offers is installed.")}
          </p>
        ) : (
          <ul className="max-h-80 divide-y divide-border overflow-y-auto rounded-xl border border-border">
            {offered.map((e) => (
              <li key={e.id} className="flex items-center gap-3 px-4 py-2">
                <span className="min-w-0 flex-1 truncate text-sm text-fg">{e.id}</span>
                <Button
                  size="sm"
                  variant="outline"
                  disabled={installing !== null}
                  onClick={() => void install(e.id)}
                  aria-label={t("Install {name}", { name: e.id })}
                >
                  <Download size={13} /> {installing === e.id ? t("Installing…") : t("Install")}
                </Button>
              </li>
            ))}
          </ul>
        )}
      </section>
    </div>
  );
}

/** Upload or paste one definition. */
export function UploadDrawer({
  open,
  onClose,
  onInstalled,
}: {
  open: boolean;
  onClose: () => void;
  onInstalled: (def: SiteDefinition) => void;
}) {
  const [yaml, setYaml] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);
  async function install() {
    setBusy(true);
    setError(null);
    try {
      const def = await api.sites.upload(yaml);
      toast.success(t("{name} installed", { name: def.name }), {
        description: def.warnings?.length ? def.warnings.join(" · ") : undefined,
      });
      setYaml("");
      onInstalled(def);
    } catch (e) {
      setError(e);
    } finally {
      setBusy(false);
    }
  }
  return (
    <Drawer
      open={open}
      onClose={() => !busy && onClose()}
      title={t("Upload definition")}
      description={t(
        "A site definition in the Cardigann YAML format, for a site you have the right to use.",
      )}
      size="lg"
      footer={
        <div className="flex w-full justify-end gap-2">
          <Button variant="ghost" size="sm" onClick={onClose} disabled={busy}>
            {t("Cancel")}
          </Button>
          <Button size="sm" onClick={() => void install()} disabled={busy || !yaml.trim()}>
            <Upload size={14} /> {busy ? t("Installing…") : t("Install definition")}
          </Button>
        </div>
      }
    >
      <div className="space-y-4">
        {error ? <MutationError error={error} /> : null}
        <SettingsRow stacked title={t("Definition file")}>
          <input
            type="file"
            accept=".yml,.yaml,text/yaml"
            aria-label={t("Definition file")}
            className="text-sm text-fg-muted"
            onChange={(e) => {
              const file = e.currentTarget.files?.[0];
              if (file) void file.text().then(setYaml);
            }}
          />
        </SettingsRow>
        <SettingsRow stacked title={t("Or paste it")}>
          <Textarea
            aria-label={t("Definition YAML")}
            rows={14}
            className="font-mono text-xs"
            spellCheck={false}
            value={yaml}
            onChange={(e) => setYaml(e.currentTarget.value)}
            placeholder={"id: example\nname: Example\nlinks:\n  - https://example.invalid/\n…"}
          />
        </SettingsRow>
      </div>
    </Drawer>
  );
}

/**
 * Site definitions, edited in place: what is installed, the definitions no
 * indexer uses yet (each can be added or removed), where definitions come
 * from, and FlareSolverr for sites behind a browser challenge. A definition
 * an indexer uses is shown in that indexer's drawer instead, so a site is
 * never listed twice on the page.
 */
export function SiteDefinitions({
  cfg,
  definitions,
  definitionsError,
  inUse,
  onAdd,
  onUpload,
  onConfigSaved,
  onDefinitionsChanged,
}: {
  cfg: EtherConfig;
  definitions: SiteDefinition[] | null;
  definitionsError: unknown;
  /** Definition ids configured indexers use. */
  inUse: Set<string>;
  /** Start a new indexer from an installed definition. */
  onAdd: (def: SiteDefinition) => void;
  onUpload: () => void;
  onConfigSaved: () => Promise<void> | void;
  onDefinitionsChanged: () => void;
}) {
  const saved = useMemo<IndexerSitesConfig>(
    () => ({
      definitionsUrl: cfg.indexerSites?.definitionsUrl ?? "",
      flareSolverrUrl: cfg.indexerSites?.flareSolverrUrl ?? "",
    }),
    [cfg.indexerSites?.definitionsUrl, cfg.indexerSites?.flareSolverrUrl],
  );
  const { draft, setDraft, dirty, reset } = useDraft<IndexerSitesConfig>(saved);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [updating, setUpdating] = useState(false);

  async function save() {
    if (!draft) return;
    setSaving(true);
    setError(null);
    setErrors({});
    try {
      await api.putSection("indexerSites", draft);
      toast.success(t("Settings saved"), { description: t("Site definitions") });
      await onConfigSaved();
    } catch (e) {
      setError(e);
      setErrors(recordErrors(e, "indexerSites."));
    } finally {
      setSaving(false);
    }
  }

  async function update() {
    setUpdating(true);
    try {
      const rep = await api.sites.update();
      const parts = [
        t("{count} updated", { count: formatNumber(rep.updated.length) }),
        t("{count} unchanged", { count: formatNumber(rep.unchanged) }),
        rep.failed.length ? t("{count} failed", { count: formatNumber(rep.failed.length) }) : "",
      ].filter(Boolean);
      if (rep.failed.length)
        toast.error(t("Some definitions could not be updated"), {
          description: rep.failed.map((f) => `${f.id}: ${f.error}`).join(" · "),
        });
      else toast.success(t("Definitions updated"), { description: parts.join(" · ") });
      onDefinitionsChanged();
    } catch (e) {
      toastError(t("Couldn't update definitions"), e);
    } finally {
      setUpdating(false);
    }
  }

  async function remove(def: SiteDefinition) {
    const ok = await confirmDialog({
      title: t("Remove definition?"),
      message: t("{name} will no longer be available for new indexers.", { name: def.name }),
      confirmLabel: t("Remove"),
      tone: "danger",
    });
    if (!ok) return;
    try {
      await api.sites.remove(def.id);
      toast.success(t("{name} removed", { name: def.name }));
      onDefinitionsChanged();
    } catch (e) {
      toastError(t("Couldn't remove {name}", { name: def.name }), e);
    }
  }

  if (!draft) return null;
  const installed = definitions ?? [];
  const fromSource = installed.some((d) => d.source === "source");
  const used = installed.filter((d) => inUse.has(d.id)).length;
  const unused = installed.filter((d) => !inUse.has(d.id));
  const summary =
    definitions === null
      ? t("Checking installed definitions…")
      : installed.length === 0
        ? t(
            "None installed yet. Upload one, or set a definitions source and pick a site when you add an indexer.",
          )
        : used === 0
          ? tn(installed.length, "{count} definition installed.", "{count} definitions installed.")
          : used === installed.length
            ? tn(
                installed.length,
                "{count} definition installed, all used by your indexers above.",
                "{count} definitions installed, all used by your indexers above.",
              )
            : tn(
                installed.length,
                "{count} definition installed, {used} used by your indexers above.",
                "{count} definitions installed, {used} used by your indexers above.",
                { used },
              );
  return (
    <>
      <SettingsSection title={t("Site definitions")}>
        {saved.definitionsUrl?.trim() ? null : (
          <EmptyState
            icon={<Upload />}
            title={t("Add a definitions source to search tracker sites")}
          />
        )}
        <SettingsRow
          title={t("Installed")}
          description={summary}
          control={
            <span className="flex flex-wrap gap-2">
              <Button size="sm" variant="outline" onClick={onUpload}>
                <Upload size={14} /> {t("Upload definition")}
              </Button>
              <Button
                size="sm"
                variant="outline"
                onClick={() => void update()}
                disabled={updating || !fromSource}
                title={fromSource ? undefined : t("No installed definition came from a source")}
              >
                <RefreshCw size={14} /> {updating ? t("Updating…") : t("Update definitions")}
              </Button>
            </span>
          }
        />
        {definitionsError ? (
          <SettingsRow stacked title={t("Couldn't load definitions")}>
            <ErrorPanel error={definitionsError} onRetry={onDefinitionsChanged} />
          </SettingsRow>
        ) : unused.length ? (
          <SettingsRow
            stacked
            title={t("Not added as an indexer")}
            description={t("Installed, but not searched until you add them.")}
          >
            <ul
              aria-label={t("Unused definitions")}
              className="divide-y divide-border rounded-xl border border-border"
            >
              {unused.map((d) => (
                <li key={d.id} className="flex flex-wrap items-center gap-3 px-4 py-2.5">
                  <div className="min-w-0 flex-1 basis-48">
                    <p className="truncate text-sm font-medium text-fg">{d.name}</p>
                    <p className="text-xs text-fg-muted">
                      {[d.id, definitionOrigin(d), d.login ? t("Needs an account") : ""]
                        .filter(Boolean)
                        .join(" · ")}
                    </p>
                    <DefinitionWarnings def={d} />
                  </div>
                  <span className="flex flex-wrap items-center gap-2">
                    <DefinitionBadges def={d} />
                  </span>
                  <span className="flex items-center gap-1">
                    <Button
                      size="sm"
                      variant="outline"
                      onClick={() => onAdd(d)}
                      aria-label={t("Add {name} as an indexer", { name: d.name })}
                    >
                      <Plus size={13} /> {t("Add as indexer")}
                    </Button>
                    <Button
                      size="sm"
                      variant="ghost"
                      className="text-danger"
                      onClick={() => void remove(d)}
                      aria-label={t("Remove {name}", { name: d.name })}
                    >
                      {t("Remove")}
                    </Button>
                  </span>
                </li>
              ))}
            </ul>
          </SettingsRow>
        ) : null}
        <TextRow
          label={t("Definitions source")}
          type="url"
          value={draft.definitionsUrl ?? ""}
          placeholder="https://…/definitions/"
          error={errors.definitionsUrl}
          help={t(
            "Optional. A folder of Cardigann-format definitions you trust: a GitHub folder or raw folder URL, a GitHub contents API URL, or a plain list of definition URLs. Ether lists it when you add a site and updates installed definitions from it.",
          )}
          onChange={(v) => setDraft({ ...draft, definitionsUrl: v })}
        />
        <TextRow
          label="FlareSolverr"
          type="url"
          value={draft.flareSolverrUrl ?? ""}
          placeholder="http://localhost:8191"
          error={errors.flareSolverrUrl}
          help={t(
            "Optional. Passes Cloudflare browser challenges for sites that use them. Without it, those sites report that they need FlareSolverr.",
          )}
          onChange={(v) => setDraft({ ...draft, flareSolverrUrl: v })}
        />
      </SettingsSection>
      <SaveBar
        dirty={dirty}
        saving={saving}
        error={error}
        onSave={save}
        onDiscard={() => {
          reset();
          setError(null);
          setErrors({});
        }}
      />
    </>
  );
}

/** Shown in a site indexer's drawer when its definition is gone. */
export function MissingDefinition({ id }: { id?: string }) {
  return (
    <Alert variant="warning" role="status">
      <AlertDescription>
        {t(
          "The definition “{id}” is not installed, so this site cannot be searched. Upload it again under Site definitions, or remove this indexer.",
          { id: id || t("unknown") },
        )}
      </AlertDescription>
    </Alert>
  );
}
