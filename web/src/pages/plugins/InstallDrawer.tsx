import { Badge, Button, Input, Switch, toast } from "@braedonsaunders/appkit-ui";
import { AlertTriangle, ArrowLeft, Link2, Upload } from "lucide-react";
import { useEffect, useState } from "react";
import { Artwork } from "../../components/Artwork";
import { Drawer } from "../../components/Drawer";
import { MutationError } from "../../components/feedback";
import { Field } from "../../components/fields";
import { SettingsRow } from "../../components/SettingsRow";
import { t } from "../../i18n";
import { apiFetch } from "../../lib/api";
import { invalidateRegistry } from "../../lib/extend";
import { PermissionSheet, RuntimeLimitsCard } from "./PermissionSheet";
import {
  isPrivateUrl,
  kindIcon,
  kindLabel,
  type InstallPreview,
  type PluginSummary,
} from "./types";

export function InstallDrawer({
  open,
  initialUrl = "",
  mode = "link",
  onClose,
  onInstalled,
}: {
  open: boolean;
  /** "file" installs a plugin package from this computer; "link" from an address. */
  mode?: "link" | "file";
  /** Prefills the address, for entries picked in a plugin directory. */
  initialUrl?: string;
  onClose: () => void;
  onInstalled: (pluginId: string) => void;
}) {
  const [url, setUrl] = useState(initialUrl);
  useEffect(() => {
    if (open && initialUrl) setUrl(initialUrl);
  }, [open, initialUrl]);
  const [lan, setLan] = useState(false);
  const [pkgFile, setPkgFile] = useState<{ name: string; bytes: ArrayBuffer } | null>(null);
  const [preview, setPreview] = useState<InstallPreview | null>(null);
  const [viewers, setViewers] = useState<number[]>([]);
  const [folders, setFolders] = useState<string[]>([]);
  const [shadow, setShadow] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);

  const reset = () => {
    setUrl("");
    setLan(false);
    setPkgFile(null);
    setPreview(null);
    setViewers([]);
    setFolders([]);
    setShadow(false);
    setBusy(false);
    setError(null);
  };

  const handleClose = () => {
    reset();
    onClose();
  };

  const privateAddr = isPrivateUrl(url);

  const runPreview = async () => {
    setBusy(true);
    setError(null);
    try {
      let res: InstallPreview;
      if (pkgFile) {
        res = await apiFetch<InstallPreview>("/plugins/install/preview", {
          method: "POST",
          headers: { "Content-Type": "application/octet-stream" },
          body: pkgFile.bytes,
        });
      } else {
        res = await apiFetch<InstallPreview>("/plugins/install/preview", {
          method: "POST",
          body: JSON.stringify({
            url: url.trim(),
            lan: privateAddr ? lan : undefined,
          }),
        });
      }
      setPreview(res);
    } catch (e) {
      setError(e);
    } finally {
      setBusy(false);
    }
  };

  const runInstall = async () => {
    if (!preview) return;
    setBusy(true);
    setError(null);
    try {
      let installed: PluginSummary;
      if (pkgFile) {
        const form = new FormData();
        form.append("package", new Blob([pkgFile.bytes]), pkgFile.name);
        form.append(
          "options",
          JSON.stringify({
            grants: { viewers, folders },
            shadow,
            acceptAddedPermissions: !!preview.addedPermissions?.length,
          }),
        );
        installed = await apiFetch<PluginSummary>("/plugins/install", {
          method: "POST",
          body: form,
        });
      } else {
        installed = await apiFetch<PluginSummary>("/plugins/install", {
          method: "POST",
          body: JSON.stringify({
            url: url.trim(),
            lan: privateAddr ? lan : undefined,
            grants: {
              viewers: preview.needsViewers ? viewers : undefined,
              folders: preview.needsFolders ? folders : undefined,
              lan: privateAddr && lan ? true : undefined,
            },
            shadow,
            acceptAddedPermissions:
              preview.addedPermissions && preview.addedPermissions.length > 0 ? true : undefined,
          }),
        });
      }
      invalidateRegistry();
      toast.success(t("Installed {name}", { name: installed.name }));
      const targetId = installed.id;
      reset();
      onInstalled(targetId);
    } catch (e) {
      setError(e);
    } finally {
      setBusy(false);
    }
  };

  const canPreview = Boolean(pkgFile || url.trim().length > 0);
  const summary =
    preview?.summary ||
    preview?.manifest.ether?.summary ||
    preview?.manifest.description ||
    t("No description provided.");
  const kind = preview?.manifest.ether?.kind;

  return (
    <Drawer
      open={open}
      onClose={handleClose}
      title={mode === "file" ? t("Upload a plugin") : t("Install from a link")}
      description={t("Paste a manifest link, a GitHub repository, or upload a package file.")}
      size="lg"
      footer={
        <div className="flex w-full items-center justify-between gap-2">
          <div>
            {preview ? (
              <Button
                type="button"
                variant="ghost"
                size="sm"
                disabled={busy}
                onClick={() => {
                  setPreview(null);
                  setError(null);
                }}
              >
                <ArrowLeft size={14} /> {t("Back")}
              </Button>
            ) : null}
          </div>
          <div className="flex items-center gap-2">
            <Button type="button" variant="ghost" disabled={busy} onClick={handleClose}>
              {t("Cancel")}
            </Button>
            {preview ? (
              <Button type="button" disabled={busy} onClick={() => void runInstall()}>
                {busy ? t("Installing…") : t("Install")}
              </Button>
            ) : (
              <Button
                type="button"
                disabled={busy || !canPreview}
                onClick={() => void runPreview()}
              >
                {busy ? t("Checking…") : t("Review permissions")}
              </Button>
            )}
          </div>
        </div>
      }
    >
      <div className="space-y-5">
        <MutationError error={error} />

        {!preview ? (
          <div className="space-y-5">
            {mode === "link" ? (
              <>
                <Field
                  label={t("Plugin address")}
                  hint={t("Paste a manifest.json URL, a Stremio link, or a GitHub repository URL.")}
                  htmlFor="plugin-install-url"
                >
                  <div className="relative">
                    <Link2
                      size={15}
                      className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-fg-subtle"
                      aria-hidden
                    />
                    <Input
                      id="plugin-install-url"
                      aria-label={t("Plugin address")}
                      value={url}
                      onChange={(e) => {
                        setUrl(e.currentTarget.value);
                        if (e.currentTarget.value.trim()) setPkgFile(null);
                      }}
                      placeholder={t(
                        "https://github.com/example/plugin or https://…/manifest.json",
                      )}
                      className="pl-9 font-mono text-xs"
                      spellCheck={false}
                      onKeyDown={(e) => {
                        if (e.key === "Enter" && canPreview && !busy) {
                          e.preventDefault();
                          void runPreview();
                        }
                      }}
                    />
                  </div>
                </Field>

                {privateAddr ? (
                  <div className="rounded-lg border border-border bg-bg-subtle px-4 py-3">
                    <SettingsRow
                      title={
                        <label htmlFor="plugin-install-lan">
                          {t("Allow local network address")}
                        </label>
                      }
                      description={t(
                        "Confirm that this plugin runs on a device you trust on your LAN.",
                      )}
                      control={
                        <Switch
                          id="plugin-install-lan"
                          checked={lan}
                          onChange={(e) => setLan(e.currentTarget.checked)}
                          aria-label={t("Allow local network address")}
                        />
                      }
                    />
                  </div>
                ) : null}
              </>
            ) : (
              <div className="rounded-xl border border-dashed border-border bg-bg-subtle p-4">
                <div className="flex flex-wrap items-center justify-between gap-3">
                  <div className="min-w-0">
                    <p className="text-sm font-medium text-fg">{t("Plugin package")}</p>
                    <p className="text-xs text-fg-muted">
                      {pkgFile
                        ? t("Selected {name}", { name: pkgFile.name })
                        : t("A .etherpkg or .zip plugin package from your computer.")}
                    </p>
                  </div>
                  <label className="inline-flex cursor-pointer items-center gap-1.5 rounded-md border border-border bg-surface px-3 py-1.5 text-xs font-medium text-fg shadow-xs hover:bg-surface-hover">
                    <Upload size={14} aria-hidden />
                    <span>{pkgFile ? t("Change file") : t("Choose a file")}</span>
                    <input
                      type="file"
                      accept=".etherpkg,.zip,application/octet-stream,application/zip"
                      aria-label={t("Upload .etherpkg")}
                      className="sr-only"
                      onChange={(e) => {
                        const file = e.currentTarget.files?.[0];
                        if (!file) return;
                        void file.arrayBuffer().then((bytes) => {
                          setPkgFile({ name: file.name, bytes });
                          setUrl("");
                        });
                      }}
                    />
                  </label>
                </div>
              </div>
            )}
          </div>
        ) : (
          <div className="space-y-5">
            <div className="flex items-start gap-3.5 rounded-xl border border-border bg-surface p-4">
              <Artwork
                src={preview.manifest.ether?.icon ?? preview.manifest.logo}
                alt={preview.manifest.name}
                title=""
                icon={kindIcon(kind, 18)}
                className="size-10 shrink-0 rounded-lg border border-border"
              />
              <div className="min-w-0 flex-1 space-y-1">
                <div className="flex flex-wrap items-center gap-2">
                  <h3 className="text-base font-semibold text-fg">{preview.manifest.name}</h3>
                  <Badge variant="secondary">{kindLabel(kind, preview.stremioOnly)}</Badge>
                  <span className="font-mono text-xs text-fg-muted">
                    v{preview.manifest.version}
                  </span>
                  {!preview.verified ? (
                    <Badge variant="warning">
                      <AlertTriangle size={12} aria-hidden /> {t("Unverified")}
                    </Badge>
                  ) : null}
                </div>
                <p className="text-sm text-fg-muted">{summary}</p>
                {preview.manifest.ether?.author?.name ? (
                  <p className="text-xs text-fg-subtle">
                    {t("By {author}", { author: preview.manifest.ether.author.name })}
                  </p>
                ) : null}
              </div>
            </div>

            <RuntimeLimitsCard
              limits={preview.runtimeLimits}
              requested={preview.manifest.ether?.runtime?.limits}
            />
            <PermissionSheet
              permissions={preview.permissions}
              verified={preview.verified}
              needsViewers={preview.needsViewers}
              needsFolders={preview.needsFolders}
              viewers={viewers}
              onChangeViewers={setViewers}
              folders={folders}
              onChangeFolders={setFolders}
              addedPermissions={preview.addedPermissions}
            />

            <div className="rounded-xl border border-border bg-surface px-4 py-2">
              <SettingsRow
                title={<label htmlFor="install-shadow-mode">{t("Shadow mode")}</label>}
                description={t(
                  "Run this plugin in shadow mode first so its changes are recorded without touching your collections or watch state.",
                )}
                control={
                  <Switch
                    id="install-shadow-mode"
                    checked={shadow}
                    onChange={(e) => setShadow(e.currentTarget.checked)}
                    aria-label={t("Shadow mode")}
                  />
                }
              />
            </div>
          </div>
        )}
      </div>
    </Drawer>
  );
}
