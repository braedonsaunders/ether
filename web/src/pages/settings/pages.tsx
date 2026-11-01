import {
  Button,
  SettingsSection,
  SubtabNav,
  buttonVariants,
  toast,
} from "@braedonsaunders/appkit-ui";
import { useCallback, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { useApp } from "../../app-context";
import { ConfigSection } from "../../components/ConfigSection";
import { DomainPage, useRouteTab } from "../../components/layout";
import { RestartButton } from "../../components/RestartDrawer";
import { SettingsRow } from "../../components/SettingsRow";
import { localized, t } from "../../i18n";
import { api } from "../../lib/api";
import { useAsync } from "../../lib/hooks";
import Logs from "../Logs";
import { BackupsAndImport } from "./Backups";
import { ClusterOverview, useClustered } from "./Cluster";
import { OutboundInternet } from "./OutboundInternet";
import { ConnectedServices, integrationTabs, type ServiceTab } from "./ConnectedServices";
import { type FieldGroup } from "./SchemaForm";
import * as S from "./schema";
import { UpdatesStatus } from "./sections";

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

function ServerStatus() {
  const { boot } = useApp();
  const name = boot.serverName || "Ether";
  return (
    <p className="text-sm text-fg-muted">
      {boot.setupComplete
        ? t("{name} is ready for viewers.", { name })
        : t("{name} still needs setup.", { name })}
    </p>
  );
}

/**
 * Names, how apps connect, then downloads, personalization and trailers.
 * Every form stays mounted across tabs, so edits on several tabs save together.
 */
export function GeneralPage() {
  const { reloadBoot } = useApp();
  const clustered = useClustered();
  const tabs = [
    { key: "server", label: t("Server") },
    ...GENERAL_SECTIONS.map((x) => ({ key: x.key, label: x.label })),
  ];
  const tab = useRouteTab(tabs);
  const toServer = useRevealErrors("/server", serverTab);
  return (
    <DomainPage title={t("General")} base="/server" tabs={tabs} active={tab}>
      <ConfigSection
        configKey="server"
        endpoint="server"
        groups={clustered ? S.clusterServerGroups() : S.SERVER}
        omit={clustered ? undefined : ["clusterWeight"]}
        label={t("General")}
        hidden={tab !== "server"}
        before={() => <ServerStatus />}
        onFieldErrors={toServer}
        onSaved={() => void reloadBoot()}
      />
      <OutboundInternet hidden={tab !== "server"} />
      {tab === "server" ? <RunSetupWizard /> : null}
      {GENERAL_SECTIONS.map((x) => (
        <GeneralSection key={x.key} section={x} hidden={tab !== x.key} />
      ))}
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
      before={section.key === "extras" ? () => <ExtrasStatus /> : undefined}
      onFieldErrors={reveal}
      onSaved={onSaved}
    />
  );
}

function ExtrasStatus() {
  const config = useAsync(() => api.getConfig(), []);
  const extras = config.data?.extras;
  const command = String(extras?.resolverCommand ?? "").trim();
  if (!extras?.enabled || command) return null;
  return (
    <p role="status" className="text-sm text-fg-muted">
      {t("Trailers are on, but no resolver command is set, so none can play.")}
    </p>
  );
}

export function ServicesPage() {
  const [enabled, setEnabled] = useState<Partial<Record<ServiceTab, boolean>>>({});
  const tabs = integrationTabs(enabled);
  const tab = useRouteTab(tabs);
  return (
    <DomainPage title={t("Integrations")} base="/services" tabs={tabs} active={tab}>
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
      actions={<RestartButton />}
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
      description={t("Servers that share one database.")}
      base="/cluster"
    >
      <ClusterOverview />
    </DomainPage>
  );
}
