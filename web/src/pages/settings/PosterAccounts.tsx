import { IntegrationSetupFooter } from "../../components/IntegrationSetupFooter";
import { Badge, Button, Input, SettingsSection, toast } from "@braedonsaunders/appkit-ui";
import { ArrowRight } from "lucide-react";
import { useEffect, useState, type ReactNode } from "react";
import { Drawer } from "../../components/Drawer";
import { MutationError } from "../../components/feedback";
import { Field, SecretInput } from "../../components/fields";
import { IntegrationAppCard } from "../../components/IntegrationAppCard";
import { IntegrationTab } from "../../components/IntegrationTab";
import { IntegrationMark } from "../../components/integration-brands";
import { SaveBar } from "../../components/SaveBar";
import { localized, t } from "../../i18n";
import { api, apiFetch, errorMessage } from "../../lib/api";
import { useAsync } from "../../lib/hooks";
import type { PosterProviderConfig as Posters } from "../../lib/types";
const emptyPoster: Posters = {
  enabled: false,
  source: "tmdb",
  baseUrl: "",
  apiKey: "",
  urlTemplate: "",
  profile: "",
};
export const POSTER_SERVICES = localized(() => [
  {
    id: "postersplus",
    name: "PostersPlus",
    description: t("Alternate posters from your PostersPlus server."),
    key: true,
    template: false,
  },
  {
    id: "xrdb",
    name: "XRDB",
    description: t("Posters from an XRDB server."),
    key: true,
    template: false,
  },
  {
    id: "betterposters",
    name: "BetterPosters",
    description: t("Styled posters from BetterPosters."),
    key: false,
    template: false,
  },
  {
    id: "rpdb",
    name: "RPDB",
    description: t("Rating posters from Rating Poster Database."),
    key: true,
    template: false,
  },
  {
    id: "custom",
    name: t("Custom posters"),
    description: t("Posters from a URL template you provide."),
    key: false,
    template: true,
  },
]);

export function normalizePosters(value?: Posters | null): Posters {
  const p = { ...emptyPoster, ...value };
  if (!p.version)
    return {
      ...emptyPoster,
      version: 1,
      assetFolder: p.assetFolder,
      ownerEnabled: p.ownerEnabled,
      source: p.enabled ? p.source : "tmdb",
      accounts: p.enabled ? { [p.source]: p } : {},
    };
  return {
    ...p,
    accounts: Object.fromEntries(
      Object.entries(p.accounts ?? {}).map(([id, a]) => [id, { ...emptyPoster, ...a, source: id }]),
    ),
  };
}
function PosterAccountForm({
  value,
  onChange,
}: {
  value: Posters;
  onChange: (p: Posters) => void;
}) {
  const service = POSTER_SERVICES.find((s) => s.id === value.source)!;
  return (
    <SettingsSection title={undefined}>
      <div className="space-y-4 p-5">
        {["postersplus", "xrdb"].includes(value.source) && (
          <Field label={t("Server URL")} htmlFor="poster-url">
            <Input
              id="poster-url"
              value={value.baseUrl}
              onChange={(e) => onChange({ ...value, baseUrl: e.target.value })}
            />
          </Field>
        )}
        {value.source === "xrdb" && (
          <Field label={t("Profile")} htmlFor="poster-profile">
            <Input
              id="poster-profile"
              value={value.profile ?? ""}
              onChange={(e) => onChange({ ...value, profile: e.target.value })}
            />
          </Field>
        )}
        {(service.key || service.template) && (
          <Field label={t("API key")} htmlFor="poster-key">
            <SecretInput
              id="poster-key"
              value={value.apiKey}
              onChange={(apiKey) => onChange({ ...value, apiKey })}
            />
          </Field>
        )}
        {(service.template || value.source === "betterposters") && (
          <Field label={t("Poster URL template")} htmlFor="poster-template">
            <SecretInput
              id="poster-template"
              value={value.urlTemplate}
              onChange={(urlTemplate) => onChange({ ...value, urlTemplate })}
            />
          </Field>
        )}
      </div>
    </SettingsSection>
  );
}
export function PosterAccounts({
  locked,
  children,
  tab = "overview",
  onEnabled,
}: {
  locked: boolean;
  children?: (cards: ReactNode, savedNote: ReactNode) => ReactNode;
  tab?: string;
  onEnabled?: (flags: Record<string, boolean>) => void;
}) {
  const config = useAsync(async () => normalizePosters((await api.getConfig()).posters), []);
  const [draft, setDraft] = useState<Posters | null>(null);
  const [editing, setEditing] = useState<Posters | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [saving, setSaving] = useState(false);
  const [saved, setSaved] = useState(false);
  useEffect(() => {
    if (config.data)
      onEnabled?.(
        Object.fromEntries(
          POSTER_SERVICES.map((s) => [s.id, !!config.data?.accounts?.[s.id]?.enabled]),
        ),
      );
  }, [config.data, onEnabled]);
  useEffect(() => {
    setEditing(config.data?.accounts?.[tab] ?? null);
  }, [tab, config.data]);
  useEffect(() => {
    setSaved(false);
  }, [tab]);
  async function save(account: Posters) {
    if (!config.data) return;
    setSaving(true);
    setError(null);
    try {
      const latest = normalizePosters((await api.getConfig()).posters);
      const next = {
        ...latest,
        accounts: { ...latest.accounts, [account.source]: account },
      };
      if (!account.enabled && next.source === account.source) next.source = "tmdb";
      const result = await apiFetch<{ value: Posters }>("/config/posters", {
        method: "PUT",
        body: JSON.stringify(posterPayload(next)),
      });
      config.setData(normalizePosters(result.value));
      setDraft(null);
      setSaved(true);
      toast.success(t("Poster provider saved"));
    } catch (e) {
      setError(e);
      if (!draft && !editing) {
        toast.error(t("Couldn't save the change"), { description: errorMessage(e) });
      }
    } finally {
      setSaving(false);
    }
  }
  const accountOf = (id: string) => ({
    ...emptyPoster,
    ...config.data?.accounts?.[id],
    source: id,
  });
  // A disabled provider has no tab yet: its setup opens the shared drawer form.
  const openSetup = (id: string) => setDraft({ ...accountOf(id), enabled: true });
  const cards = POSTER_SERVICES.map((s) => (
    <IntegrationAppCard
      key={s.id}
      identity={{ id: s.id, name: s.name }}
      logo={<IntegrationMark id={s.id} />}
      description={s.description}
      enabled={!!config.data?.accounts?.[s.id]?.enabled}
      disabled={locked || saving || !config.data}
      status={
        <Badge variant={config.data?.accounts?.[s.id]?.enabled ? "success" : "secondary"}>
          {config.data?.accounts?.[s.id]?.enabled ? t("On") : t("Off")}
        </Badge>
      }
      onToggle={(on) => (on ? openSetup(s.id) : void save({ ...accountOf(s.id), enabled: false }))}
      configureHref={config.data?.accounts?.[s.id]?.enabled ? "/services/" + s.id : undefined}
      configureLabel={t("Manage {app}", { app: s.name })}
      action={
        config.data?.accounts?.[s.id]?.enabled ? undefined : (
          <Button
            size="sm"
            variant="outline"
            disabled={locked || saving || !config.data}
            onClick={() => openSetup(s.id)}
          >
            {t("Set up {app}", { app: s.name })} <ArrowRight size={14} />
          </Button>
        )
      }
    />
  ));
  const savedNote = saved ? (
    <p className="text-sm text-fg-muted" role="status">
      {t("Apps reload posters the next time they open a library; some may need to be restarted.")}
    </p>
  ) : null;
  const service = POSTER_SERVICES.find((s) => s.id === tab);
  return (
    <>
      {children?.(cards, savedNote)}
      {editing && service && (
        <>
          <IntegrationTab
            id={tab}
            name={service.name}
            enabled={!!config.data?.accounts?.[tab]?.enabled}
            onToggle={(on) => void save({ ...editing, enabled: on })}
            disabled={saving || locked}
            href="/metadata/artwork"
            usage={t("Use {app} posters in Metadata › Artwork.", {
              app: service.name,
            })}
            current={config.data?.source === tab}
          >
            <PosterAccountForm value={editing} onChange={setEditing} />
            {savedNote}
          </IntegrationTab>
          <SaveBar
            dirty={JSON.stringify(editing) !== JSON.stringify(config.data?.accounts?.[tab])}
            saving={saving}
            error={error}
            onSave={() => save(editing)}
            onDiscard={() => setEditing(config.data?.accounts?.[tab] ?? null)}
          />
        </>
      )}
      <Drawer
        open={!!draft}
        onClose={() => {
          if (!saving) setDraft(null);
        }}
        title={t("Set up {app}", {
          app: POSTER_SERVICES.find((s) => s.id === draft?.source)?.name ?? "",
        })}
        description={t("Save and enable turns this on. Cancel leaves it off.")}
        footer={
          <IntegrationSetupFooter
            disabled={saving}
            onCancel={() => setDraft(null)}
            onSave={() => {
              if (draft) void save(draft);
            }}
          />
        }
      >
        {error ? <MutationError error={error} /> : null}
        {draft && <PosterAccountForm value={draft} onChange={setDraft} />}
      </Drawer>
    </>
  );
}

export function posterPayload(p: Posters) {
  return {
    ...p,
    accounts: Object.fromEntries(
      Object.entries(p.accounts ?? {}).map(([id, a]) => [
        id,
        {
          enabled: a.enabled,
          baseUrl: a.baseUrl,
          apiKey: a.apiKey,
          urlTemplate: a.urlTemplate,
          profile: a.profile ?? "",
        },
      ]),
    ),
  };
}
