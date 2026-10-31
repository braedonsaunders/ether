import {
  Button,
  EmptyState,
  Input,
  SearchSelect,
  SettingsSection,
  confirmDialog,
} from "@braedonsaunders/appkit-ui";
import { ListFilter, Plus } from "lucide-react";
import { useState } from "react";
import { Link } from "react-router-dom";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { Section } from "../../components/layout";
import { useViewers, viewerName } from "../../components/pickers";
import { SettingsRow } from "../../components/SettingsRow";
import { localized, t } from "../../i18n";
import type { Viewer } from "../../lib/contract";
import type { QualityRule } from "../../lib/types";
import {
  OrderButtons,
  RecordDrawer,
  RecordItem,
  recordErrors,
  swap,
  toastError,
  type Errors,
} from "../sources/form";
import { searchSelectLabels } from "../../i18n/appkit-labels";
import { useQuality } from "./useQuality";

const DEVICES = [
  "Plex for Apple TV",
  "Plex Web",
  "Plex for iOS",
  "Plex for Android",
  "Roku",
  "Fire TV",
  "LG",
  "Samsung",
  "Xbox",
  "PlayStation",
];
const CUSTOM = "__custom__";

const HEIGHTS = localized(() => [
  { value: "0", label: t("Any") },
  { value: "480", label: "480p" },
  { value: "720", label: "720p" },
  { value: "1080", label: "1080p" },
  { value: "2160", label: t("4K (2160p)") },
]);

const heightLabel = (h: number) => HEIGHTS.find((x) => x.value === String(h))?.label ?? `${h}p`;

function ruleViewer(r: QualityRule, viewers: Viewer[] | null) {
  if (!r.userId) return "";
  const v = viewers?.find((x) => x.id === r.userId);
  return v ? viewerName(v) : t("Unavailable viewer");
}

function ruleSummary(r: QualityRule, viewers: Viewer[] | null) {
  return [
    r.protocol,
    r.product || t("Any device"),
    r.platform,
    ruleViewer(r, viewers),
    r.maxHeight ? heightLabel(r.maxHeight) : "",
  ]
    .filter(Boolean)
    .join(" · ");
}

/** Client rules: the first rule matching an app picks its quality profile. */
export function ClientRulesTab() {
  const { config, quality, commit } = useQuality();
  const { viewers } = useViewers();
  const [editing, setEditing] = useState<{ index: number; draft: QualityRule } | null>(null);
  const [errors, setErrors] = useState<Errors>({});
  const [error, setError] = useState<unknown>(null);
  const [saving, setSaving] = useState(false);

  if (config.error) return <ErrorPanel error={config.error} onRetry={() => void config.reload()} />;
  if (!quality) return <LoadingRows rows={3} />;

  const rules = quality.rules ?? [];
  const profiles = quality.profiles ?? [];
  const names = profiles.map((p) => ({ value: p.name, label: p.name }));

  const open = (index: number, draft: QualityRule) => {
    setErrors({});
    setError(null);
    setEditing({ index, draft: { ...draft } });
  };

  async function save() {
    if (!editing || !quality) return;
    const { index, draft } = editing;
    const list =
      index < rules.length ? rules.map((r, j) => (j === index ? draft : r)) : [...rules, draft];
    setSaving(true);
    setError(null);
    setErrors({});
    try {
      await commit(
        { ...quality, rules: list },
        index < rules.length ? t("Rule saved") : t("Rule added"),
      );
      setEditing(null);
    } catch (e) {
      setError(e);
      setErrors(recordErrors(e, `quality.rules[${index}].`));
    } finally {
      setSaving(false);
    }
  }

  async function remove() {
    if (!editing || !quality) return;
    const { index } = editing;
    const ok = await confirmDialog({
      title: t("Remove rule?"),
      message: t("Apps it matched will use the next matching rule, or the defaults."),
      confirmLabel: t("Remove"),
      tone: "danger",
    });
    if (!ok) return;
    setSaving(true);
    setError(null);
    try {
      await commit({ ...quality, rules: rules.filter((_, j) => j !== index) }, t("Rule removed"));
      setEditing(null);
    } catch (e) {
      setError(e);
    } finally {
      setSaving(false);
    }
  }

  async function move(i: number, d: -1 | 1) {
    if (!quality) return;
    try {
      await commit({ ...quality, rules: swap(rules, i, i + d) }, t("Rule order saved"));
    } catch (e) {
      toastError(t("Couldn't reorder rules"), e);
    }
  }

  const draft = editing?.draft;
  const isNew = editing ? editing.index >= rules.length : false;
  const set = (patch: Partial<QualityRule>) =>
    setEditing((e) => (e ? { ...e, draft: { ...e.draft, ...patch } } : e));
  const custom =
    draft?.product !== undefined && !DEVICES.includes(draft.product) && draft.product !== "";

  return (
    <>
      <Section
        title={t("Client rules")}
        description={t(
          "Checked from the top. The first rule that matches an app picks its quality profile; apps no rule matches use the defaults.",
        )}
        actions={
          <Button
            size="sm"
            onClick={() =>
              open(rules.length, { profile: quality.default || profiles[0]?.name || "" })
            }
            disabled={!profiles.length}
          >
            <Plus size={14} /> {t("Add rule")}
          </Button>
        }
        bodyClassName=""
      >
        {rules.length === 0 ? (
          <EmptyState
            icon={<ListFilter />}
            title={t("No rules")}
            description={t("Every app uses the default quality profiles.")}
          />
        ) : (
          <ul className="divide-y divide-border">
            {rules.map((r, i) => (
              <RecordItem
                key={i}
                title={`${i + 1}. ${ruleSummary(r, viewers)}`}
                description={t("Uses {profile}", { profile: r.profile || t("no profile") })}
                order={
                  <OrderButtons
                    label={t("rule {n}", { n: i + 1 })}
                    i={i}
                    n={rules.length}
                    onMove={(d) => void move(i, d)}
                  />
                }
                editLabel={t("Edit rule {n}", { n: i + 1 })}
                onEdit={() => open(i, r)}
              />
            ))}
          </ul>
        )}
      </Section>
      <p className="text-sm text-fg-muted">
        {t(
          "Rules choose among your quality profiles. What each app can actually play is still checked before playback.",
        )}{" "}
        <Link className="text-primary underline" to="/playback/devices">
          {t("View device capabilities")}
        </Link>
      </p>

      <RecordDrawer
        open={editing !== null}
        onClose={() => !saving && setEditing(null)}
        title={isNew ? t("New rule") : t("Rule {n}", { n: (editing?.index ?? 0) + 1 })}
        description={t("Match an app by device, platform or viewer.")}
        onSave={() => void save()}
        saving={saving}
        saveLabel={t("Save rule")}
        onRemove={isNew ? undefined : () => void remove()}
        removeLabel={t("Remove rule")}
        error={error}
      >
        {draft ? (
          <SettingsSection title={t("Match")}>
            <SettingsRow
              title={t("Protocol")}
              description={t("Choose which kind of app this rule matches.")}
              control={
                <div className="w-56">
                  <SearchSelect
                    value={draft.protocol ?? ""}
                    onChange={(v) => set({ protocol: v || undefined })}
                    options={[
                      { value: "", label: t("Any protocol") },
                      { value: "plex", label: "Plex" },
                      { value: "jellyfin", label: "Jellyfin" },
                      { value: "emby", label: "Emby" },
                      { value: "silo", label: "Silo" },
                      { value: "stremio", label: "Stremio" },
                      { value: "webdav", label: t("Files") },
                      { value: "dlna", label: "DLNA" },
                    ]}
                    ariaLabel={t("Protocol")}
                    searchable={false}
                  />
                </div>
              }
            />
            <SettingsRow
              title={t("Device")}
              description={t("The app’s product name.")}
              control={
                <div className="w-56">
                  <SearchSelect
                    {...searchSelectLabels()}
                    value={custom ? CUSTOM : (draft.product ?? "")}
                    onChange={(v) =>
                      set({ product: v === CUSTOM ? "Custom device" : v || undefined })
                    }
                    options={[
                      { value: "", label: t("Any device") },
                      ...DEVICES.map((v) => ({ value: v, label: v })),
                      { value: CUSTOM, label: t("Custom product name…") },
                    ]}
                    ariaLabel={t("Device")}
                  />
                </div>
              }
            />
            {custom ? (
              <SettingsRow
                title={t("Product name contains")}
                control={
                  <Input
                    value={draft.product ?? ""}
                    onChange={(e) => set({ product: e.currentTarget.value })}
                    aria-label={t("Custom product")}
                    className="sm:w-56"
                  />
                }
              />
            ) : null}
            <SettingsRow
              title={t("Platform")}
              description={t("e.g. tvOS, Android. Empty matches any.")}
              control={
                <Input
                  value={draft.platform ?? ""}
                  placeholder={t("Any")}
                  onChange={(e) => set({ platform: e.currentTarget.value || undefined })}
                  aria-label={t("Platform")}
                  className="sm:w-56"
                />
              }
            />
            <SettingsRow
              title={t("Viewer")}
              control={
                <div className="w-56">
                  <SearchSelect
                    {...searchSelectLabels()}
                    value={String(draft.userId ?? 0)}
                    onChange={(v) => set({ userId: Number(v) || undefined })}
                    options={[
                      { value: "0", label: t("Everyone") },
                      ...(viewers ?? []).map((v) => ({
                        value: String(v.id),
                        label: viewerName(v),
                      })),
                    ]}
                    ariaLabel={t("Viewer")}
                  />
                </div>
              }
            />
            <SettingsRow
              title={t("Quality profile")}
              description={
                errors.profile ? <span className="text-danger">{errors.profile}</span> : undefined
              }
              control={
                <div className="w-56">
                  <SearchSelect
                    {...searchSelectLabels()}
                    value={draft.profile}
                    onChange={(v) => set({ profile: v })}
                    options={names}
                    ariaLabel={t("Profile")}
                    searchable={false}
                  />
                </div>
              }
            />
            <SettingsRow
              title={t("Maximum resolution")}
              description={
                errors.maxHeight ? (
                  <span className="text-danger">{errors.maxHeight}</span>
                ) : undefined
              }
              control={
                <div className="w-56">
                  <SearchSelect
                    {...searchSelectLabels()}
                    value={String(draft.maxHeight ?? 0)}
                    onChange={(v) => set({ maxHeight: Number(v) || undefined })}
                    options={HEIGHTS}
                    ariaLabel={t("Maximum resolution")}
                    searchable={false}
                  />
                </div>
              }
            />
          </SettingsSection>
        ) : null}
      </RecordDrawer>
    </>
  );
}
