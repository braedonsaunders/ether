import {
  Button,
  SettingsSection,
  Spinner,
  SubtabNav,
  confirmDialog,
  toast,
} from "@braedonsaunders/appkit-ui";
import { ExternalLink } from "lucide-react";
import { useEffect, useMemo, useRef, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { useApp } from "../../app-context";
import { MoreOptions } from "../../components/ConfigSection";
import { RemoteDiagnostics } from "../../components/RemoteDiagnostics";
import { ErrorPanel } from "../../components/feedback";
import { SaveBar, useDraft } from "../../components/SaveBar";
import { SettingsRow } from "../../components/SettingsRow";
import { t, tx } from "../../i18n";
import { ApiError, api } from "../../lib/api";
import { useAsync } from "../../lib/hooks";
import type { EtherConfig } from "../../lib/types";
import { SchemaField, type FieldDef } from "../settings/SchemaForm";
import * as schema from "../settings/schema";
import { PlexStatus } from "../settings/sections";
import { plexAccountClaimed } from "./plex-account";
import { HomeStatusRow } from "./HomeStatus";
import { awaySummary, claimSummary, type ReachabilityStatus } from "./reachability";

type Obj = Record<string, unknown>;
type PlexDraft = { port: number; bindAddress: string; plex: Obj };
type Pin = { id: string; code: string; authUrl: string };
export type PlexSection = "account" | "remote" | "compatibility";

export function plexSection(section: string | undefined): PlexSection {
  if (section === "remote" || section === "secure") return "remote";
  if (section === "compatibility") return "compatibility";
  return "account";
}

function plexGroup(id: string) {
  const group = schema.PLEX.find((item) => item.id === id);
  if (!group) throw new Error(`missing Plex settings group ${id}`);
  return group;
}

function networkFields(): FieldDef[] {
  return schema.SERVER.flatMap((group) => group.fields).filter((field) =>
    ["port", "bindAddress"].includes(field.key),
  );
}

function FieldList({
  fields,
  value,
  errors,
  prefix,
  onChange,
}: {
  fields: FieldDef[];
  value: Obj;
  errors: Record<string, string>;
  prefix: string;
  onChange: (key: string, next: unknown) => void;
}) {
  return (
    <>
      {fields.map((field) => (
        <SchemaField
          key={field.key}
          def={field}
          value={value[field.key]}
          path={`${prefix}.${field.key}`}
          error={errors[`${prefix}.${field.key}`]}
          errors={errors}
          onChange={(next) => onChange(field.key, next)}
        />
      ))}
    </>
  );
}

function PlexAccount({
  config,
  onChanged,
}: {
  config: EtherConfig;
  onChanged: () => Promise<void>;
}) {
  const plex = config.plex;
  const claimed = plexAccountClaimed(plex);
  const [pin, setPin] = useState<Pin | null>(null);
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState("");
  const [claimFailed, setClaimFailed] = useState(false);
  const mounted = useRef(true);
  const pollRef = useRef<number | null>(null);
  const pinRun = useRef(0);
  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
      pinRun.current++;
      if (pollRef.current) window.clearInterval(pollRef.current);
      pollRef.current = null;
    };
  }, []);

  async function claimServer() {
    if (!mounted.current) return;
    setBusy(true);
    try {
      await api.claim();
      if (!mounted.current) return;
      setClaimFailed(false);
      setNotice(t("Server claimed to your Plex account."));
      await onChanged();
    } catch (error) {
      if (!mounted.current) return;
      setClaimFailed(true);
      setNotice(t("Signed in, but claiming failed: {error}", { error: (error as Error).message }));
    } finally {
      if (mounted.current) setBusy(false);
    }
  }

  async function startPin() {
    const run = ++pinRun.current;
    const current = () => mounted.current && run === pinRun.current;
    if (pollRef.current) window.clearInterval(pollRef.current);
    pollRef.current = null;
    setBusy(true);
    setClaimFailed(false);
    setNotice("");
    try {
      const next = await api.pinStart();
      if (!current()) return;
      setPin(next);
      window.open(next.authUrl, "_blank", "noopener,noreferrer");
      let polling = false;
      pollRef.current = window.setInterval(() => {
        if (!current() || polling) return;
        polling = true;
        void api
          .pinPoll(next.id)
          .then(async ({ done }) => {
            if (!current() || !done) return;
            if (pollRef.current) window.clearInterval(pollRef.current);
            pollRef.current = null;
            setPin(null);
            setNotice(t("Signed in. Claiming this server…"));
            await claimServer();
          })
          .catch((error: unknown) => {
            if (!current()) return;
            if (pollRef.current) window.clearInterval(pollRef.current);
            pollRef.current = null;
            setPin(null);
            setNotice((error as Error).message);
          })
          .finally(() => {
            polling = false;
          });
      }, 2000);
    } catch (error) {
      if (current()) setNotice((error as Error).message);
    } finally {
      if (current()) setBusy(false);
    }
  }

  async function unclaim() {
    const ok = await confirmUnclaim();
    if (!ok) return;
    setBusy(true);
    setNotice("");
    try {
      const live = await api.getConfig();
      await api.putSection("plex", { ...live.plex, claimed: false });
      await onChanged();
      setNotice("");
    } catch (error) {
      setNotice((error as Error).message);
    } finally {
      setBusy(false);
    }
  }

  const owner = plex.ownerUsername;
  return (
    <>
      <SettingsSection title={t("Plex account")}>
        <SettingsRow
          title={claimed ? t("Signed in") : t("Not signed in")}
          description={
            claimed
              ? claimSummary(true, owner)
              : t(
                  "Not signed in to plex.tv. Sign in to claim this server so Plex apps can find it.",
                )
          }
          control={
            claimed ? (
              <Button variant="outline" size="sm" disabled={busy} onClick={() => void unclaim()}>
                {t("Sign out of Plex")}
              </Button>
            ) : (
              <Button size="sm" disabled={busy || pin != null} onClick={() => void startPin()}>
                {t("Sign in & claim")}
              </Button>
            )
          }
        />
        {pin ? (
          <div className="space-y-3 px-5 py-4">
            <p className="text-sm text-fg">
              {t(
                "A plex.tv window opened. Sign in there and allow Ether; this page continues on its own.",
              )}
            </p>
            <div className="flex flex-wrap items-center gap-3 text-sm text-fg-muted">
              <Button
                variant="outline"
                size="sm"
                onClick={() => window.open(pin.authUrl, "_blank", "noopener,noreferrer")}
              >
                {t("Open plex.tv sign-in")} <ExternalLink size={13} />
              </Button>
              <span className="inline-flex items-center gap-2">
                {t("Waiting for approval")} <Spinner size={13} label={t("Waiting")} />
              </span>
            </div>
            {pin.code.length <= 8 ? (
              <p className="text-sm text-fg-muted">
                {tx("Or enter {code} at plex.tv/link.", {
                  code: <span className="font-mono font-semibold text-fg">{pin.code}</span>,
                })}
              </p>
            ) : null}
          </div>
        ) : null}
        {notice ? <p className="px-5 py-3 text-sm text-fg-muted">{notice}</p> : null}
        {claimFailed && !pin ? (
          <div className="px-5 py-3">
            <Button variant="outline" size="sm" disabled={busy} onClick={() => void claimServer()}>
              {t("Claim server")}
            </Button>
          </div>
        ) : null}
      </SettingsSection>
      <SettingsSection
        title={t("Server identity")}
        description={t("These identify the server. Ether sets them; they are not edited here.")}
      >
        {owner ? <SettingsRow title={t("Owner")} description={owner} /> : null}
        <SettingsRow
          title={t("Server name")}
          description={
            <>
              <span>{config.server.friendlyName || "—"}</span>
              {" · "}
              <Link className="underline" to="/server">
                {t("General settings")}
              </Link>
            </>
          }
        />
        <SettingsRow
          title={t("Machine identifier")}
          description={config.server.machineIdentifier || "—"}
        />
      </SettingsSection>
    </>
  );
}

function confirmUnclaim() {
  return confirmDialog({
    title: t("Sign out of Plex"),
    message: t(
      "This removes the server from your Plex account. Plex apps will not see it until you sign in and claim it again.",
    ),
    confirmLabel: t("Sign out of Plex"),
    tone: "danger",
  });
}

export function PlexApp({
  hidden,
  section,
  status,
}: {
  hidden: boolean;
  section?: string;
  status?: ReachabilityStatus;
}) {
  const navigate = useNavigate();
  const { dashboard, refreshDashboard } = useApp();
  const config = useAsync(() => api.getConfig(), []);
  const saved = useMemo<PlexDraft | null>(() => {
    const cfg = config.data;
    if (!cfg) return null;
    return {
      port: cfg.server.port,
      bindAddress: cfg.server.bindAddress,
      plex: { ...cfg.plex },
    };
  }, [config.data]);
  const { draft, setDraft, dirty, reset } = useDraft(saved);
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [error, setError] = useState<unknown>();
  const [saving, setSaving] = useState(false);
  const active = plexSection(section);

  async function save() {
    if (!draft) return;
    setSaving(true);
    setError(undefined);
    setErrors({});
    try {
      const live = await api.getConfig();
      await api.putSection("server", {
        ...live.server,
        port: draft.port,
        bindAddress: draft.bindAddress,
      });
      await api.putSection("plex", draft.plex);
      toast.success(t("Settings saved"));
      await config.reload();
      void refreshDashboard();
    } catch (err) {
      setError(err);
      if (err instanceof ApiError && err.fields) {
        setErrors(Object.fromEntries(err.fields.map((field) => [field.field, field.message])));
        const keys = err.fields.map((field) => field.field);
        if (keys.some((key) => key.startsWith("server.") || key.startsWith("plex."))) {
          const compatibility = keys.every((key) =>
            [
              "plex.trackLatestVersion",
              "plex.compatibilityVersion",
              "plex.latestCompatibilityVersion",
            ].includes(key),
          );
          navigate(compatibility ? "/apps/plex/compatibility" : "/apps/plex/remote");
        }
      }
    } finally {
      setSaving(false);
    }
  }

  const saveBar = (
    <SaveBar
      dirty={dirty}
      saving={saving}
      error={error}
      onSave={save}
      onDiscard={() => {
        reset();
        setErrors({});
        setError(undefined);
      }}
    />
  );
  if (hidden) return saveBar;
  if (config.error) return <ErrorPanel error={config.error} onRetry={() => void config.reload()} />;
  if (!config.data || !draft)
    return (
      <div className="py-10 text-center">
        <Spinner size={20} label={t("Loading settings")} />
      </div>
    );

  const setPlex = (key: string, value: unknown) =>
    setDraft((old) => (old ? { ...old, plex: { ...old.plex, [key]: value } } : old));
  const setServer = (key: string, value: unknown) =>
    setDraft((old) => (old ? { ...old, [key]: value } : old));
  const remote = plexGroup("remote");
  const secure = plexGroup("secure");
  const compatibility = plexGroup("compatibility");
  const tabs = [
    { key: "account", label: t("Account") },
    { key: "remote", label: t("Remote access") },
    { key: "compatibility", label: t("Compatibility") },
  ];
  const plex = config.data.plex;
  const away = awaySummary(status, { remoteAccess: plex.remoteAccess !== false });

  return (
    <>
      <SettingsSection title={t("Status")}>
        <HomeStatusRow status={status} />
        <SettingsRow title={away.title} description={away.detail} />
        <SettingsRow
          title={t("Plex account")}
          description={claimSummary(plexAccountClaimed(plex), plex.ownerUsername)}
        />
      </SettingsSection>
      <SubtabNav
        ariaLabel={t("Plex settings")}
        active={active}
        onSelect={(key) => navigate(`/apps/plex/${key}`)}
        tabs={tabs}
      />
      {active === "account" ? (
        <PlexAccount
          config={config.data}
          onChanged={async () => {
            await config.reload();
            void refreshDashboard();
          }}
        />
      ) : null}
      {active === "remote" ? (
        <>
          <PlexStatus remote={dashboard?.sections?.remote} />
          <SettingsSection
            title={t("Network")}
            description={t("Where Plex apps on your network connect to this server.")}
          >
            <FieldList
              fields={networkFields()}
              value={{ port: draft.port, bindAddress: draft.bindAddress }}
              errors={errors}
              prefix="server"
              onChange={setServer}
            />
          </SettingsSection>
          <OptionGroup group={remote} value={draft.plex} errors={errors} onChange={setPlex} />
          <OptionGroup
            group={secure}
            value={draft.plex}
            errors={errors}
            onChange={setPlex}
            moreLabel={t("Use your own certificate")}
          />
          <RemoteDiagnostics />
        </>
      ) : null}
      {active === "compatibility" ? (
        <OptionGroup
          group={{
            ...compatibility,
            description: t(
              "What version Plex apps think this server is. Leave this alone unless an app refuses to connect.",
            ),
          }}
          value={draft.plex}
          errors={errors}
          onChange={setPlex}
        />
      ) : null}
      {saveBar}
    </>
  );
}

function OptionGroup({
  group,
  value,
  errors,
  onChange,
  moreLabel,
}: {
  group: { title?: string; description?: string; fields: FieldDef[]; more?: FieldDef[] };
  value: Obj;
  errors: Record<string, string>;
  onChange: (key: string, next: unknown) => void;
  moreLabel?: string;
}) {
  const extra = group.more ?? [];
  const label = group.title
    ? t("More options: {title}", { title: group.title })
    : t("More options");
  return (
    <SettingsSection title={group.title} description={group.description}>
      <FieldList
        fields={group.fields}
        value={value}
        errors={errors}
        prefix="plex"
        onChange={onChange}
      />
      {extra.length ? (
        <MoreOptions label={moreLabel ?? label} text={moreLabel ?? t("More options")}>
          <FieldList
            fields={extra}
            value={value}
            errors={errors}
            prefix="plex"
            onChange={onChange}
          />
        </MoreOptions>
      ) : null}
    </SettingsSection>
  );
}
