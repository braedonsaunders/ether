import {
  Alert,
  AlertDescription,
  AlertTitle,
  Badge,
  Button,
  SearchSelect,
  SettingsSection,
  Textarea,
  toast,
} from "@braedonsaunders/appkit-ui";
import { ExternalLink, FileUp, RefreshCw } from "lucide-react";
import { useEffect, useMemo, useRef, useState } from "react";
import { LoadingInline } from "../../components/feedback";
import { Segmented } from "../../components/fields";
import { SettingsRow } from "../../components/SettingsRow";
import { localized, t, tn } from "../../i18n";
import { ApiError, errorMessage, isAbort } from "../../lib/api";
import { formatNumber } from "../../lib/format";
import { useAsync } from "../../lib/hooks";
import {
  qualityGateLabel,
  releaseConsole,
  sourceLabel,
  type ImportFormat,
  type QualityImportPlan,
  type QualityImportSource,
  type StarrApp,
  type TrashProfile,
} from "../../lib/releases";
import type { QualityProfile } from "../../lib/types";
import { RecordDrawer, TextRow } from "../sources/form";
import { searchSelectLabels } from "../../i18n/appkit-labels";
import { useLocale } from "../../i18n/react";

type Mode = "guides" | "paste";

const APPS = localized(() => [
  { value: "radarr" as const, label: t("Radarr (movies)") },
  { value: "sonarr" as const, label: t("Sonarr (shows)") },
]);

const MODES = localized(() => [
  { value: "guides" as const, label: "TRaSH Guides" },
  { value: "paste" as const, label: t("Paste JSON") },
]);

/** A pasted profile is a few KiB; the server takes up to 1 MiB. */
const MAX_PASTE = 1 << 20;

/** "SQP", "German", "Anime" from "[SQP] SQP-1 (2160p)"; the rest are TRaSH's standard profiles. */
function profileGroup(name: string): string {
  const tag = /^\[([^\]]+)\]/.exec(name)?.[1];
  return tag ? (tag.split(/\s+/)[0] ?? tag) : t("Standard");
}

function heightLabel(h: number): string {
  return h >= 2160 ? "4K" : `${h}p`;
}

function resolution(p: QualityProfile): string {
  return p.minHeight === p.maxHeight
    ? heightLabel(p.maxHeight)
    : t("{min} to {max}", { min: heightLabel(p.minHeight), max: heightLabel(p.maxHeight) });
}

function signed(n: number): string {
  return n > 0 ? `+${formatNumber(n)}` : formatNumber(n);
}

const STATUS: Record<
  ImportFormat["status"],
  { label: string; variant: "secondary" | "success" | "warning" }
> = localized(() => ({
  present: { label: t("Already in Custom scoring"), variant: "secondary" },
  add: { label: t("Added"), variant: "success" },
  unsupported: { label: t("Can't import"), variant: "warning" },
  missing: { label: t("Not found"), variant: "warning" },
}));

/**
 * Import a Radarr or Sonarr quality profile from TRaSH Guides, or from pasted
 * profile JSON: preview what Ether makes of it, then add it as a new profile.
 */
export function TrashImport({
  open,
  onClose,
  onImported,
}: {
  open: boolean;
  onClose: () => void;
  onImported: () => Promise<void> | void;
}) {
  const [app, setApp] = useState<StarrApp>("radarr");
  const [mode, setMode] = useState<Mode>("guides");
  const [profileId, setProfileId] = useState("");
  const [pasted, setPasted] = useState("");
  const [plan, setPlan] = useState<QualityImportPlan | null>(null);
  const [source, setSource] = useState<QualityImportSource | null>(null);
  const [name, setName] = useState("");
  const [previewing, setPreviewing] = useState(false);
  const [previewError, setPreviewError] = useState<unknown>(null);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<unknown>(null);
  const [nameError, setNameError] = useState<string>();
  const preview = useRef<AbortController | null>(null);
  const file = useRef<HTMLInputElement>(null);

  const listing = open && mode === "guides";
  const profiles = useAsync(
    (signal) =>
      listing ? releaseConsole.trashProfiles(app, signal) : Promise.resolve<TrashProfile[]>([]),
    [listing, app],
  );
  const list = useMemo(() => (listing ? (profiles.data ?? []) : []), [listing, profiles.data]);
  const { locale } = useLocale();
  const options = useMemo(
    () =>
      list.map((p) => ({
        value: p.id,
        label: p.name,
        group: profileGroup(p.name),
        hint: tn(p.formats, "{count} custom format", "{count} custom formats"),
      })),
    [list, locale],
  );
  const chosen = list.find((p) => p.id === profileId);

  // Start over each time the drawer opens.
  useEffect(() => {
    if (!open) return;
    setProfileId("");
    setPasted("");
    clearPreview();
    setSaveError(null);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  function clearPreview() {
    preview.current?.abort();
    setPlan(null);
    setSource(null);
    setPreviewError(null);
    setPreviewing(false);
    setNameError(undefined);
  }

  async function load(next: QualityImportSource) {
    preview.current?.abort();
    const ctl = new AbortController();
    preview.current = ctl;
    setPlan(null);
    setSource(null);
    setPreviewError(null);
    setSaveError(null);
    setNameError(undefined);
    setPreviewing(true);
    try {
      const p = await releaseConsole.previewImport(next, ctl.signal);
      if (ctl.signal.aborted) return;
      setPlan(p);
      setSource({ ...next, app: p.app } as QualityImportSource);
      setName(p.profile.name);
      // Pasted JSON can name the other app; follow the server's reading.
      if (p.app !== next.app) setApp(p.app);
    } catch (e) {
      if (!isAbort(e) && !ctl.signal.aborted) setPreviewError(e);
    } finally {
      if (preview.current === ctl) setPreviewing(false);
    }
  }

  function pick(id: string) {
    setProfileId(id);
    if (id) void load({ app, profile: id });
    else clearPreview();
  }

  async function upload(f: File | undefined) {
    if (!f) return;
    if (f.size > MAX_PASTE) {
      setPreviewError(
        new Error(t("That file is larger than 1 MiB; choose a TRaSH quality profile file.")),
      );
      return;
    }
    const text = await f.text();
    setPasted(text);
    void load({ app, json: text });
  }

  async function save() {
    if (!plan || !source) return;
    const finalName = name.trim();
    if (!finalName) {
      setNameError(t("Enter a name."));
      return;
    }
    setSaving(true);
    setSaveError(null);
    setNameError(undefined);
    try {
      await releaseConsole.importProfile(source, finalName);
      toast.success(t("Profile added"));
      await onImported();
      onClose();
    } catch (e) {
      if (e instanceof ApiError && e.code === "exists") setNameError(e.message);
      else setSaveError(e);
    } finally {
      setSaving(false);
    }
  }

  return (
    <RecordDrawer
      open={open}
      onClose={() => !saving && onClose()}
      title={t("Import from TRaSH Guides")}
      description={t(
        "Turn a Radarr or Sonarr quality profile from TRaSH Guides into an Ether profile, custom format scores included.",
      )}
      onSave={() => void save()}
      saving={saving}
      saveLabel={t("Add profile")}
      saveDisabled={!plan || previewing}
      error={saveError}
    >
      <SettingsSection title={t("Profile to import")}>
        <SettingsRow
          title={t("App")}
          description={t("Movies use Radarr's guide; shows use Sonarr's.")}
          control={
            <Segmented
              ariaLabel={t("App")}
              size="sm"
              value={app}
              options={APPS}
              onChange={(v) => {
                setApp(v);
                setProfileId("");
                clearPreview();
              }}
            />
          }
        />
        <SettingsRow
          title={t("Import from")}
          description={t("Paste or upload a profile file if GitHub is unreachable.")}
          control={
            <Segmented
              ariaLabel={t("Import from")}
              size="sm"
              value={mode}
              options={MODES}
              onChange={(v) => {
                setMode(v);
                setProfileId("");
                clearPreview();
              }}
            />
          }
        />
        {mode === "guides" ? (
          <SettingsRow
            stacked
            title={t("TRaSH profile")}
            description={
              profiles.error ? (
                <span className="text-danger">{errorMessage(profiles.error)}</span>
              ) : (
                t("Profiles TRaSH Guides publishes, refreshed hourly.")
              )
            }
          >
            <div className="space-y-2">
              {profiles.error ? (
                <Button variant="outline" size="sm" onClick={() => void profiles.reload()}>
                  <RefreshCw size={14} /> {t("Try again")}
                </Button>
              ) : (
                <SearchSelect
                  {...searchSelectLabels()}
                  value={profileId}
                  onChange={pick}
                  options={options}
                  ariaLabel={t("TRaSH profile")}
                  placeholder={profiles.loading ? t("Loading profiles…") : t("Choose a profile")}
                  searchPlaceholder={t("Search profiles")}
                  sheetTitle={t("TRaSH profile")}
                  loading={profiles.loading}
                  disabled={profiles.loading && list.length === 0}
                  searchable
                />
              )}
              {chosen?.description ? (
                <p className="whitespace-pre-line text-xs text-fg-muted">{chosen.description}</p>
              ) : null}
              {chosen?.url?.startsWith("https://trash-guides.info/") ? (
                <a
                  href={chosen.url}
                  target="_blank"
                  rel="noreferrer noopener"
                  className="inline-flex items-center gap-1 text-xs text-primary underline"
                >
                  {t("Read the guide")} <ExternalLink size={12} />
                </a>
              ) : null}
            </div>
          </SettingsRow>
        ) : (
          <SettingsRow
            stacked
            title={t("Profile JSON")}
            description={t(
              "A quality profile file from TRaSH Guides (docs/json/radarr or sonarr/quality-profiles).",
            )}
          >
            <div className="space-y-2">
              <Textarea
                aria-label={t("Profile JSON")}
                className="min-h-40 w-full font-mono text-xs"
                spellCheck={false}
                value={pasted}
                placeholder='{ "name": "WEB 1080p", "items": [ … ], "formatItems": { … } }'
                onChange={(e) => {
                  setPasted(e.target.value);
                  if (plan || previewError) clearPreview();
                }}
              />
              <div className="flex flex-wrap gap-2">
                <Button
                  size="sm"
                  variant="outline"
                  disabled={!pasted.trim() || previewing || pasted.length > MAX_PASTE}
                  onClick={() => void load({ app, json: pasted })}
                >
                  {t("Preview")}
                </Button>
                <Button size="sm" variant="ghost" onClick={() => file.current?.click()}>
                  <FileUp size={14} /> {t("Upload file")}
                </Button>
                <input
                  ref={file}
                  type="file"
                  accept="application/json,.json"
                  className="hidden"
                  aria-label={t("Profile file")}
                  onChange={(e) => {
                    void upload(e.currentTarget.files?.[0]);
                    e.currentTarget.value = "";
                  }}
                />
              </div>
            </div>
          </SettingsRow>
        )}
      </SettingsSection>

      {previewing ? (
        <LoadingInline label={t("Reading the profile and its custom formats…")} />
      ) : null}
      {previewError ? (
        <Alert variant="destructive" role="alert">
          <AlertTitle>{t("Couldn't read that profile")}</AlertTitle>
          <AlertDescription>{errorMessage(previewError)}</AlertDescription>
        </Alert>
      ) : null}
      {plan ? (
        <ImportPreview plan={plan} name={name} nameError={nameError} onName={setName} />
      ) : null}
    </RecordDrawer>
  );
}

function ImportPreview({
  plan,
  name,
  nameError,
  onName,
}: {
  plan: QualityImportPlan;
  name: string;
  nameError?: string;
  onName: (v: string) => void;
}) {
  const p = plan.profile;
  const counts = plan.formats.reduce<Record<string, number>>((acc, f) => {
    acc[f.status] = (acc[f.status] ?? 0) + 1;
    return acc;
  }, {});
  const skipped = (counts.unsupported ?? 0) + (counts.missing ?? 0);
  const formatSummary = [
    counts.add ? tn(counts.add, "{count} added", "{count} added") : "",
    counts.present
      ? tn(counts.present, "{count} already in Custom scoring", "{count} already in Custom scoring")
      : "",
    skipped ? tn(skipped, "{count} can't be imported", "{count} can't be imported") : "",
  ]
    .filter(Boolean)
    .join(" · ");
  return (
    <>
      <SettingsSection
        title={t("New profile")}
        description={t("Edit it like any other profile once added.")}
      >
        <TextRow
          label={t("Name")}
          value={name}
          error={nameError}
          onChange={onName}
          help={t("Must differ from your other profiles.")}
        />
        <SettingsRow stacked title={t("Allowed qualities")}>
          <ul className="flex flex-wrap gap-1.5" aria-label={t("Allowed qualities")}>
            {(p.allowedQualities ?? []).map((q) => (
              <li key={q}>
                <Badge variant="outline">{qualityGateLabel(q)}</Badge>
              </li>
            ))}
          </ul>
        </SettingsRow>
        <SettingsRow
          title={t("Resolution")}
          control={<span className="text-sm text-fg">{resolution(p)}</span>}
        />
        <SettingsRow
          title={t("Sources")}
          description={t("In order of preference.")}
          control={
            <span className="text-sm text-fg">
              {(p.preferredSources ?? []).map(sourceLabel).join(" › ")}
            </span>
          }
        />
        <SettingsRow
          title={t("HDR and Dolby Vision")}
          description={t("Used only on apps that support them.")}
          control={
            <span className="text-sm text-fg">
              {p.allowDolbyVision ? t("Allowed") : p.allowHdr ? t("HDR allowed") : t("SDR only")}
            </span>
          }
        />
        <SettingsRow
          title={t("Minimum format score")}
          description={t("Releases scoring lower aren't played.")}
          control={
            <span className="text-sm tabular-nums text-fg">
              {formatNumber(p.minCustomFormatScore ?? 0)}
            </span>
          }
        />
        {p.preferredLanguages?.length ? (
          <SettingsRow
            title={t("Preferred language")}
            control={
              <span className="text-sm capitalize text-fg">{p.preferredLanguages.join(", ")}</span>
            }
          />
        ) : null}
      </SettingsSection>

      <SettingsSection
        title={t("Custom formats")}
        description={
          plan.formats.length
            ? `${formatSummary}.${plan.scoreSet ? ` ${t("Scored with TRaSH's {set} scores.", { set: plan.scoreSet })}` : ""}`
            : t("This profile scores no custom formats.")
        }
      >
        {plan.formats.length ? (
          <div className="max-h-96 overflow-auto rounded-lg border border-border">
            <table className="w-full text-left text-sm" aria-label={t("Custom formats")}>
              <thead className="sticky top-0 bg-bg-subtle text-xs text-fg-muted">
                <tr>
                  <th className="px-3 py-2 font-medium">{t("Format")}</th>
                  <th className="px-3 py-2 text-right font-medium">{t("Score")}</th>
                  <th className="px-3 py-2 font-medium">{t("Status")}</th>
                </tr>
              </thead>
              <tbody>
                {plan.formats.map((f) => (
                  <tr className="border-t border-border align-top" key={f.id}>
                    <td className="px-3 py-2">
                      <span className="block text-fg">{f.name}</span>
                      {f.reason ? (
                        <span className="block text-xs text-fg-muted">{f.reason}</span>
                      ) : null}
                    </td>
                    <td
                      className={`px-3 py-2 text-right tabular-nums ${f.score < 0 ? "text-danger" : "text-fg"} ${
                        f.status === "unsupported" || f.status === "missing"
                          ? "line-through opacity-60"
                          : ""
                      }`}
                    >
                      {signed(f.score)}
                    </td>
                    <td className="px-3 py-2">
                      <Badge variant={STATUS[f.status].variant}>{STATUS[f.status].label}</Badge>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : null}
      </SettingsSection>

      {plan.warnings.length ? (
        <Alert variant="warning">
          <AlertTitle>{t("Worth knowing")}</AlertTitle>
          <AlertDescription>
            <ul className="mt-1 list-disc space-y-1 pl-5">
              {plan.warnings.map((w) => (
                <li key={w}>{w}</li>
              ))}
            </ul>
          </AlertDescription>
        </Alert>
      ) : null}
    </>
  );
}
