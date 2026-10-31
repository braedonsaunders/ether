import {
  Button,
  SettingsSection,
  SubtabNav,
  buttonVariants,
  toast,
} from "@braedonsaunders/appkit-ui";
import { useCallback, useMemo, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { useApp } from "../../app-context";
import { ConfigSection } from "../../components/ConfigSection";
import { ErrorPanel } from "../../components/feedback";
import { DomainPage, useRouteTab } from "../../components/layout";
import { SettingsRow } from "../../components/SettingsRow";
import { localized, t } from "../../i18n";
import { api } from "../../lib/api";
import { humanizeKey, isPlainObject } from "../../lib/format";
import { useAsync } from "../../lib/hooks";
import Logs from "../Logs";
import { BackupsAndImport } from "./Backups";
import { ClusterOverview, useClustered } from "./Cluster";
import { ConnectedServices, integrationTabs, type ServiceTab } from "./ConnectedServices";
import { inferField, type FieldGroup } from "./SchemaForm";
import * as S from "./schema";
import { UpdatesStatus } from "./sections";

/**
 * config.json sections with a home somewhere in the UI. Any other object the
 * server reports gets its own tab on General so new options are never unreachable.
 */
const KNOWN_KEYS = new Set([
  "frontends",
  "frontendPorts",
  "server",
  "plex",
  "usenet",
  "indexers",
  "indexerSites",
  "debrid",
  "cache",
  "sharedCache", // Cache owns shared storage settings.
  "availability", // Sources owns availability synchronization.
  "catalog",
  "resolver",
  "quality",
  "transcode",
  "trakt",
  "simkl",
  "openSubtitles",
  "llm",
  "updates",
  "spike",
  "markers",
  "downloads",
  "personalize",
  "extras",
  "posters", // Metadata → Artwork owns the provider editor.
  // Node-local storage, chosen at startup; the admin API shows it redacted
  // and never edits it, so it gets no generic form on General.
  "database",
  "setupComplete",
]);

type Obj = Record<string, unknown>;

/** Reveals the tab holding the first field the server rejected. */
function useRevealErrors(base: string, tabOf: (key: string) => string | undefined) {
  const navigate = useNavigate();
  return useCallback(
    (keys: string[]) => {
      const tab = keys.map(tabOf).find(Boolean);
      if (tab) navigate(`${base}/${tab}`);
    },
    [base, navigate, tabOf],
  );
}

const serverTab = () => "server";

/** General sections other than the server itself, each on its own tab. */
const GENERAL_SECTIONS = localized(
  () =>
    [
      { key: "downloads", label: t("Downloads"), groups: S.DOWNLOADS },
      { key: "personalize", label: t("Personalization"), groups: S.PERSONALIZE },
      { key: "extras", label: t("Trailers & extras"), groups: S.EXTRAS },
    ] as const,
);

/**
 * Server name, port, limits and logging, then downloads, personalization and
 * trailers, plus any config section this UI has no page for. Every form stays
 * mounted across tabs, so edits on several tabs save together.
 */
export function GeneralPage() {
  const { reloadBoot } = useApp();
  const clustered = useClustered();
  const config = useAsync(() => api.getConfig(), []);
  const cfg = config.data as Obj | null | undefined;
  const extras = useMemo(
    () => Object.keys(cfg ?? {}).filter((k) => !KNOWN_KEYS.has(k) && isPlainObject(cfg?.[k])),
    [cfg],
  );
  const tabs = [
    { key: "server", label: t("Server") },
    ...GENERAL_SECTIONS.map((x) => ({ key: x.key, label: x.label })),
    ...extras.map((k) => ({ key: k, label: humanizeKey(k) })),
  ];
  const tab = useRouteTab(tabs);
  const toServer = useRevealErrors("/server", serverTab);
  return (
    <DomainPage
      title={t("General")}
      description={t("Server name, port and limits, downloads, personalization and trailers.")}
      base="/server"
      tabs={tabs}
      active={tab}
    >
      <ConfigSection
        configKey="server"
        endpoint="server"
        groups={clustered ? S.clusterServerGroups() : S.SERVER}
        // Node weight only matters in a cluster.
        omit={clustered ? undefined : ["clusterWeight"]}
        label={t("General")}
        hidden={tab !== "server"}
        onFieldErrors={toServer}
        onSaved={() => void reloadBoot()}
      />
      {tab === "server" ? <RunSetupWizard /> : null}
      {GENERAL_SECTIONS.map((x) => (
        <GeneralSection key={x.key} section={x} hidden={tab !== x.key} />
      ))}
      {config.error ? (
        <ErrorPanel error={config.error} onRetry={() => void config.reload()} />
      ) : null}
      {extras.map((key) => {
        const value = cfg?.[key] as Obj;
        return (
          <GeneralSection
            key={key}
            section={{
              key,
              label: humanizeKey(key),
              groups: [
                {
                  title: humanizeKey(key),
                  fields: Object.keys(value).map((k) => inferField(k, value[k])),
                },
              ],
            }}
            hidden={tab !== key}
            onSaved={() => void config.reload()}
          />
        );
      })}
    </DomainPage>
  );
}

function RunSetupWizard() {
  const navigate = useNavigate();
  const { boot, reloadBoot } = useApp();
  const [busy, setBusy] = useState(false);
  const reopen = async () => {
    setBusy(true);
    try {
      // Completed setup stays complete. Reopen does not wipe configuration;
      // clearing setupComplete would reopen tokenless LAN access.
      await api.setupReopen();
      await reloadBoot();
      navigate("/setup");
    } catch (e) {
      toast.error(t("Couldn't reopen setup"), { description: (e as Error).message });
    } finally {
      setBusy(false);
    }
  };
  return (
    <SettingsSection
      title={t("Setup")}
      description={t("Open the setup wizard. Saved configuration stays in place.")}
    >
      <SettingsRow
        title={t("Setup wizard")}
        description={
          boot.setupComplete
            ? t("Walk through setup again without erasing this server's settings.")
            : t("Finish choosing apps and configuring your server.")
        }
        control={
          boot.setupComplete ? (
            <Button variant="outline" size="sm" disabled={busy} onClick={() => void reopen()}>
              {t("Run setup wizard")}
            </Button>
          ) : (
            <Link to="/setup" className={buttonVariants({ variant: "outline", size: "sm" })}>
              {t("Run setup wizard")}
            </Link>
          )
        }
      />
    </SettingsSection>
  );
}

function GeneralSection({
  section,
  hidden,
  onSaved,
}: {
  section: { key: string; label: string; groups: readonly FieldGroup[] };
  hidden: boolean;
  onSaved?: () => void;
}) {
  const tabOf = useCallback(() => section.key, [section.key]);
  const reveal = useRevealErrors("/server", tabOf);
  return (
    <ConfigSection
      configKey={section.key}
      groups={[...section.groups]}
      label={section.label}
      hidden={hidden}
      onFieldErrors={reveal}
      onSaved={onSaved}
    />
  );
}

export function ServicesPage() {
  const [enabled, setEnabled] = useState<Partial<Record<ServiceTab, boolean>>>({});
  const tabs = integrationTabs(enabled);
  const tab = useRouteTab(tabs);
  return (
    <DomainPage
      title={t("Integrations")}
      description={t(
        "Every integration Ether uses and how it's doing. Turn one on and its settings open on a tab here.",
      )}
      base="/services"
      tabs={tabs}
      active={tab}
    >
      <ConnectedServices tab={tab} onEnabled={setEnabled} />
    </DomainPage>
  );
}

const BACKUP_TABS = localized(
  () =>
    [
      { key: "backups", label: t("Backups") },
      { key: "import", label: t("Import & export") },
    ] as const,
);

const MAINTENANCE_TABS = localized(
  () =>
    [
      { key: "backups", label: t("Backups & import") },
      { key: "updates", label: t("Updates") },
      { key: "troubleshooting", label: t("Troubleshooting") },
      { key: "logs", label: t("Logs") },
    ] as const,
);

export function MaintenancePage() {
  const tab = useRouteTab(MAINTENANCE_TABS);
  return (
    <DomainPage
      title={t("Maintenance")}
      description={t("Back up and import your data, manage updates and troubleshoot your server.")}
      base="/maintenance"
      tabs={MAINTENANCE_TABS}
      active={tab}
    >
      {tab === "backups" ? <BackupsSettings /> : null}
      {tab === "updates" ? (
        <ConfigSection
          configKey="updates"
          endpoint="updates"
          groups={S.UPDATES}
          label={t("Updates")}
          before={() => <UpdatesStatus />}
        />
      ) : null}
      {tab === "troubleshooting" ? (
        <ConfigSection
          configKey="spike"
          endpoint="spike"
          groups={S.SPIKE}
          label={t("Troubleshooting")}
        />
      ) : null}
      {tab === "logs" ? <Logs /> : null}
    </DomainPage>
  );
}

function BackupsSettings() {
  const { backupTab } = useParams();
  const navigate = useNavigate();
  const tab = backupTab === "import" ? "import" : "backups";
  return (
    <>
      <SubtabNav
        ariaLabel={t("Backups & import sections")}
        tabs={[...BACKUP_TABS]}
        active={tab}
        onSelect={(key) => navigate(`/maintenance/backups/${key}`)}
      />
      <BackupsAndImport tab={tab} />
    </>
  );
}

export function ClusterPage() {
  return (
    <DomainPage
      title={t("Cluster")}
      description={t("Ether servers sharing one database and appearing in Plex as one server.")}
      base="/cluster"
    >
      <ClusterOverview />
    </DomainPage>
  );
}
