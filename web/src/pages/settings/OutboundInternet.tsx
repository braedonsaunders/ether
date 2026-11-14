import {
  Badge,
  Button,
  Input,
  SearchSelect,
  SettingsSection,
  Spinner,
  Switch,
  toast,
} from "@braedonsaunders/appkit-ui";
import { useMemo, useState } from "react";
import { ErrorPanel } from "../../components/feedback";
import { ListInput, SecretInput } from "../../components/fields";
import { MoreOptions } from "../../components/ConfigSection";
import { SaveBar, useDraft } from "../../components/SaveBar";
import { SettingsRow } from "../../components/SettingsRow";
import { t } from "../../i18n";
import { api, ApiError } from "../../lib/api";
import { useAsync } from "../../lib/hooks";
import type { EtherConfig } from "../../lib/types";

interface OutboundConfig {
  enabled: boolean;
  preset?: string;
  url: string;
  username?: string;
  password?: string;
  indexers: boolean;
  stremio: boolean;
  debrid: boolean;
  usenet: boolean;
  metadata: boolean;
  bypass?: string[] | null;
}

export interface OutboundCheck {
  sourceIp?: string;
  sourceError?: string;
  publishedIp?: string;
  publishedError?: string;
  remoteAccess?: boolean;
  proxyEnabled?: boolean;
}

function defaults(saved: EtherConfig["outbound"]): OutboundConfig {
  return {
    enabled: saved?.enabled ?? false,
    preset: saved?.preset ?? "",
    url: saved?.url ?? "",
    username: saved?.username ?? "",
    password: saved?.password ?? "",
    indexers: saved?.indexers ?? true,
    stremio: saved?.stremio ?? true,
    debrid: saved?.debrid ?? true,
    usenet: saved?.usenet ?? true,
    metadata: saved?.metadata ?? false,
    bypass: saved?.bypass ?? [],
  };
}

const ipAddress = /^(?:\d{1,3}\.){3}\d{1,3}$|^[0-9a-f:]+$/i;

/** What the check result means, so the page can show one translated sentence. */
export function outboundVerdict(r: OutboundCheck): "split" | "same" | "down" | "source" | "off" {
  if (!r.proxyEnabled) return "off";
  if (r.sourceError || !r.sourceIp) return "down";
  if (r.publishedIp && ipAddress.test(r.sourceIp) && ipAddress.test(r.publishedIp)) {
    return r.sourceIp === r.publishedIp ? "same" : "split";
  }
  return "source";
}

export function OutboundInternet({ hidden = false }: { hidden?: boolean }) {
  const config = useAsync(() => api.getConfig(), []);
  const saved = useMemo(() => (config.data ? defaults(config.data.outbound) : null), [config.data]);
  const { draft, setDraft, dirty, reset } = useDraft(saved);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const [fields, setFields] = useState<Record<string, string>>({});
  const [check, setCheck] = useState<OutboundCheck | null>(null);
  const [checking, setChecking] = useState(false);

  const saveBar = (
    <SaveBar
      dirty={dirty}
      saving={saving}
      error={error}
      onDiscard={() => {
        reset();
        setError(null);
        setFields({});
      }}
      onSave={async () => {
        if (!draft) return;
        setSaving(true);
        setError(null);
        setFields({});
        try {
          await api.putSection("outbound", draft);
          toast.success(t("Saved"));
          await config.reload();
        } catch (e) {
          if (e instanceof ApiError && e.fields?.length) {
            const next: Record<string, string> = {};
            for (const f of e.fields) next[f.field] = f.message;
            setFields(next);
          }
          setError(e);
        } finally {
          setSaving(false);
        }
      }}
    />
  );
  if (hidden) return saveBar;
  if (config.error) return <ErrorPanel error={config.error} onRetry={() => void config.reload()} />;
  if (!draft) {
    return (
      <div className="py-6 text-center">
        <Spinner size={20} label={t("Loading settings")} />
      </div>
    );
  }

  const patch = (part: Partial<OutboundConfig>) => setDraft({ ...draft, ...part });
  const runCheck = async () => {
    setChecking(true);
    setCheck(null);
    try {
      setCheck(await api.checkOutbound(draft));
    } catch (e) {
      setCheck({
        proxyEnabled: draft.enabled,
        sourceError: e instanceof Error ? e.message : t("Couldn't check the proxy"),
      });
    } finally {
      setChecking(false);
    }
  };

  return (
    <SettingsSection
      title={t("How Ether reaches the internet")}
      description={t(
        "Use a proxy from your VPN container for sources. Plex, updates and your router stay on your normal connection.",
      )}
    >
      <SettingsRow
        setting="send-indexers-debrid-and-usenet-through-a-proxy"
        title={t("Send indexers, debrid and Usenet through a proxy")}
        description={t(
          "Off until you turn it on. A proxy that is down is not replaced by a direct connection.",
        )}
        control={
          <Switch
            aria-label={t("Send indexers, debrid and Usenet through a proxy")}
            checked={draft.enabled}
            onChange={(e) => patch({ enabled: e.currentTarget.checked })}
          />
        }
      />
      <div className="space-y-3 border-t border-border p-4">
        <label
          htmlFor="outbound-proxy"
          className="block text-sm font-medium"
          data-setting="proxy-address"
        >
          {t("Proxy address")}
        </label>
        <div className="flex flex-wrap items-center gap-2">
          <Input
            id="outbound-proxy"
            className="min-w-48 flex-1"
            aria-label={t("Proxy address")}
            aria-invalid={Boolean(fields["outbound.url"]) || undefined}
            value={draft.url}
            placeholder="http://127.0.0.1:8888"
            spellCheck={false}
            onChange={(e) =>
              patch({ url: e.currentTarget.value, preset: draft.preset || "custom" })
            }
          />
          <SearchSelect
            ariaLabel={t("Common proxy setups")}
            value=""
            placeholder={t("Common setups")}
            searchable={false}
            onChange={(url) => patch({ url, preset: "gluetun" })}
            options={[
              { value: "http://127.0.0.1:8888", label: t("VPN container on this computer") },
              {
                value: "http://gluetun:8888",
                label: t("VPN container (Gluetun) on a shared Docker network"),
              },
              { value: "socks5://gluetun:1080", label: t("Gluetun with SOCKS enabled") },
            ]}
          />
        </div>
        {fields["outbound.url"] ? (
          <p role="alert" className="text-sm">
            {fields["outbound.url"]}
          </p>
        ) : null}
        <Button
          type="button"
          size="sm"
          variant="outline"
          disabled={checking}
          onClick={() => void runCheck()}
        >
          {checking ? t("Checking…") : t("Test connection")}
        </Button>
        {check ? <CheckResult result={check} /> : null}
      </div>
      <MoreOptions
        setting="how-ether-reaches-the-internet"
        label={t("More options: How Ether reaches the internet")}
      >
        <SettingsRow
          setting="username"
          title={t("Username")}
          control={
            <Input
              aria-label={t("Username")}
              value={draft.username ?? ""}
              autoComplete="off"
              spellCheck={false}
              onChange={(e) => patch({ username: e.currentTarget.value })}
            />
          }
        />
        <SettingsRow
          setting="password"
          title={t("Password")}
          control={
            <SecretInput
              ariaLabel={t("Password")}
              value={draft.password ?? ""}
              onChange={(password) => patch({ password })}
            />
          }
        />
        <RouteSwitch
          label={t("Indexers and built-in sites")}
          checked={draft.indexers}
          onChange={(indexers) => patch({ indexers })}
        />
        <RouteSwitch
          label={t("Stremio addons, plugins and Live TV/IPTV")}
          checked={draft.stremio}
          onChange={(stremio) => patch({ stremio })}
        />
        <RouteSwitch
          label={t("Debrid services and streams")}
          checked={draft.debrid}
          onChange={(debrid) => patch({ debrid })}
        />
        <RouteSwitch
          label={t("Usenet")}
          checked={draft.usenet}
          onChange={(usenet) => patch({ usenet })}
        />
        <RouteSwitch
          label={t("Metadata, artwork, subtitles and the language model")}
          checked={draft.metadata}
          onChange={(metadata) => patch({ metadata })}
        />
        <SettingsRow
          setting="names-that-stay-on-this-network"
          title={t("Names that stay on this network")}
          description={t(
            "Hostnames or addresses, separated by commas. This computer and private addresses always stay here.",
          )}
          control={
            <ListInput
              ariaLabel={t("Names that stay on this network")}
              value={draft.bypass ?? []}
              onChange={(bypass) => patch({ bypass: bypass.map(String) })}
            />
          }
        />
      </MoreOptions>
      {saveBar}
    </SettingsSection>
  );
}

function RouteSwitch({
  label,
  checked,
  onChange,
}: {
  label: string;
  checked: boolean;
  onChange: (v: boolean) => void;
}) {
  return (
    <SettingsRow
      title={label}
      control={
        <Switch
          aria-label={label}
          checked={checked}
          onChange={(e) => onChange(e.currentTarget.checked)}
        />
      }
    />
  );
}

function CheckResult({ result }: { result: OutboundCheck }) {
  const verdict = outboundVerdict(result);
  return (
    <div className="space-y-2 rounded-lg border border-border bg-surface p-3 text-sm" role="status">
      <Badge>
        {verdict === "down"
          ? t("Connection failed")
          : verdict === "off"
            ? t("Proxy is off")
            : t("Connection successful")}
      </Badge>
      {result.publishedIp ? <p>{t("Plex publishes {ip}", { ip: result.publishedIp })}</p> : null}
      {result.publishedError ? <p>{result.publishedError}</p> : null}
      {result.sourceIp ? <p>{t("Sources will use {ip}", { ip: result.sourceIp })}</p> : null}
      {verdict === "split" ? <p>{t("Split tunnel working")}</p> : null}
      {verdict === "same" ? (
        <p>
          {t(
            "Sources and Plex are using the same address. A split tunnel should show two different addresses.",
          )}
        </p>
      ) : null}
      {verdict === "down" ? (
        <p>
          {result.sourceError ||
            t("The proxy did not answer. Sources stay off that route until it does.")}
        </p>
      ) : null}
    </div>
  );
}
