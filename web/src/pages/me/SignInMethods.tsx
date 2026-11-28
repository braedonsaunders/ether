import { t } from "../../i18n";
import { formatDateTime } from "../../lib/format";
import { useCallback, useEffect, useRef, useState, type ReactNode, type FormEvent } from "react";
import {
  Alert,
  AlertDescription,
  Button,
  EmptyState,
  Input,
  SettingsSection,
} from "@braedonsaunders/appkit-ui";
import { KeyRound, MonitorSmartphone } from "lucide-react";
import { LoadingRows } from "../../components/feedback";
import { PasswordInput } from "../../components/fields";
import { SettingsRow } from "../../components/SettingsRow";

export const MIN_LOCAL_PASSWORD = 12;

type Method = { id: number; provider: string; display: string };
type Device = {
  hash: string;
  surface: string;
  client: string;
  product: string;
  version: string;
  lastUsed: string | number;
};

function providerLabel(provider: string): string {
  switch (provider.toLowerCase()) {
    case "plex":
      return t("Plex");
    case "local":
      return t("Username & password");
    case "invite":
      return t("Joined with an invite link");
    case "oidc":
      return t("Single sign-on");
    default:
      return provider;
  }
}

function deviceName(device: Device): string {
  return device.client || device.product || device.surface || t("Device");
}

function lastUsedLabel(value: string | number): string {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "";
  return t("Last used {when}", { when: formatDateTime(date) });
}

export default function SignInMethods({
  base,
  token,
  owner = false,
  self = !owner,
  showInvite = false,
  beforeDevices,
  quickConnect,
  oidcName = null,
}: {
  base: string;
  token: string;
  owner?: boolean;
  /** True when showing the signed-in person's own account rather than another viewer. */
  self?: boolean;
  beforeDevices?: ReactNode;
  quickConnect?: ReactNode;
  /** Invite links mint a new viewer account and stay off individual viewer profiles. */
  showInvite?: boolean;
  /** The single sign-on provider's name when it is on; viewers may link it. */
  oidcName?: string | null;
}) {
  const [methods, setMethods] = useState<Method[]>([]);
  const [devices, setDevices] = useState<Device[]>([]);
  const [loaded, setLoaded] = useState(false);
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [invite, setInvite] = useState("");
  const [error, setError] = useState("");
  const [message, setMessage] = useState("");
  const [busy, setBusy] = useState(false);
  const [pinURL, setPinURL] = useState("");
  const cancelled = useRef(false);
  useEffect(() => {
    cancelled.current = false;
    return () => {
      cancelled.current = true;
    };
  }, []);
  const call = useCallback(
    async <T,>(path: string, body?: unknown): Promise<T> => {
      const res = await fetch(`/ether/api${base}${path}`, {
        method: body === undefined ? "GET" : "POST",
        headers: {
          ...(token ? { Authorization: `Bearer ${token}` } : {}),
          ...(body === undefined
            ? {}
            : { "Content-Type": "application/json", "X-Ether-Request": "1" }),
        },
        ...(body === undefined ? {} : { body: JSON.stringify(body) }),
      });
      if (!res.ok) throw new Error(t("The request could not be completed. Try again."));
      return res.json() as Promise<T>;
    },
    [base, token],
  );
  const load = useCallback(async () => {
    const [a, b] = await Promise.all([
      call<{ identities: Method[] }>("/identities"),
      call<{ devices: Device[] }>("/devices"),
    ]);
    setMethods(a.identities ?? []);
    setDevices(b.devices ?? []);
  }, [call]);
  useEffect(() => {
    let cancelled = false;
    load()
      .catch(() => {
        if (!cancelled) setError(t("Sign-in methods and devices could not be loaded."));
      })
      .finally(() => {
        if (!cancelled) setLoaded(true);
      });
    return () => {
      cancelled = true;
    };
  }, [load]);
  const localName = methods.find((m) => m.provider === "local")?.display ?? "";
  async function update(e: FormEvent) {
    e.preventDefault();
    if (password.length < MIN_LOCAL_PASSWORD) {
      setError(t("Password must be at least 12 characters."));
      return;
    }
    setBusy(true);
    setError("");
    setMessage("");
    try {
      await call("/password", { username: localName || username.trim(), password });
      setPassword("");
      setMessage(t("Password saved."));
      await load();
    } catch {
      setError(t("The request could not be completed. Try again."));
    } finally {
      setBusy(false);
    }
  }
  async function revoke(hash: string) {
    setBusy(true);
    setError("");
    try {
      await call("/devices/revoke", { hash });
      await load();
    } catch {
      setError(t("The request could not be completed. Try again."));
    } finally {
      setBusy(false);
    }
  }
  async function linkPlex() {
    setBusy(true);
    setError("");
    setMessage("");
    const popup = window.open("", "_blank", "width=560,height=720");
    try {
      const pin = await call<{ handle: string; authUrl: string }>("/link/plex", {});
      if (cancelled.current) return;
      setPinURL(pin.authUrl);
      if (popup) {
        popup.opener = null;
        popup.location.href = pin.authUrl;
      }
      const deadline = Date.now() + 10 * 60 * 1000;
      while (!cancelled.current && Date.now() < deadline) {
        await new Promise((resolve) => setTimeout(resolve, 2000));
        if (cancelled.current) return;
        const result = await call<{ done: boolean }>(
          `/link/plex/${encodeURIComponent(pin.handle)}`,
        );
        if (result.done) {
          await load();
          setMessage(t("Plex account linked."));
          return;
        }
      }
      if (!cancelled.current) setError(t("Sign-in timed out. Try again."));
    } catch {
      if (!cancelled.current) setError(t("The Plex account could not be linked."));
    } finally {
      popup?.close();
      if (!cancelled.current) {
        setPinURL("");
        setBusy(false);
      }
    }
  }
  async function linkOIDC() {
    setBusy(true);
    setError("");
    setMessage("");
    const popup = window.open("", "_blank", "width=560,height=720");
    try {
      const flow = await call<{ handle: string; authUrl: string }>("/link/oidc", {});
      if (cancelled.current) return;
      if (popup) {
        popup.opener = null;
        popup.location.href = flow.authUrl;
      }
      const deadline = Date.now() + 10 * 60 * 1000;
      while (!cancelled.current && Date.now() < deadline) {
        await new Promise((resolve) => setTimeout(resolve, 2000));
        if (cancelled.current) return;
        const result = await call<{ done: boolean }>(
          `/link/oidc/${encodeURIComponent(flow.handle)}`,
        );
        if (result.done) {
          await load();
          setMessage(t("{provider} account linked.", { provider: oidcName ?? "" }));
          return;
        }
      }
      if (!cancelled.current) setError(t("Sign-in timed out. Try again."));
    } catch {
      if (!cancelled.current)
        setError(t("The {provider} account could not be linked.", { provider: oidcName ?? "" }));
    } finally {
      popup?.close();
      if (!cancelled.current) setBusy(false);
    }
  }
  async function unlink(id: number) {
    setBusy(true);
    setError("");
    setMessage("");
    try {
      await call("/identities/unlink", { id });
      await load();
    } catch {
      setError(t("The request could not be completed. Try again."));
    } finally {
      setBusy(false);
    }
  }
  async function makeInvite() {
    setBusy(true);
    setError("");
    try {
      const result = await call<{ url: string }>("/invite", {});
      setInvite(new URL(result.url, window.location.origin).href);
    } catch {
      setError(t("The request could not be completed. Try again."));
    } finally {
      setBusy(false);
    }
  }
  const passwordTooShort = password !== "" && password.length < MIN_LOCAL_PASSWORD;
  // An accepted invite is how the account was made, not a way to sign in:
  // viewers don't see it, and the owner sees it once however many were used.
  const shown = methods.filter(
    (m, index) =>
      m.provider !== "invite" ||
      (owner && methods.findIndex((other) => other.provider === "invite") === index),
  );
  const plexLinked = methods.some((m) => m.provider === "plex");
  const oidcLinked = methods.some((m) => m.provider === "oidc");
  const canLinkOIDC = !owner && Boolean(oidcName) && !oidcLinked;
  return (
    <div className="space-y-6">
      {error ? (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      ) : null}
      {message ? (
        <Alert role="status">
          <AlertDescription>{message}</AlertDescription>
        </Alert>
      ) : null}
      <SettingsSection
        title={t("Sign-in methods")}
        description={
          owner && !self
            ? t("Accounts that can sign in as this viewer.")
            : t("Accounts that can sign in as you.")
        }
        footer={
          !owner && (!plexLinked || canLinkOIDC) ? (
            <div className="flex flex-wrap items-center gap-3">
              <span className="mr-auto text-sm text-fg-muted">
                {!plexLinked
                  ? t("Link Plex to sign in here with your Plex account next time.")
                  : t("Link {provider} to sign in here with it next time.", {
                      provider: oidcName ?? "",
                    })}
              </span>
              {pinURL ? (
                <a href={pinURL} target="_blank" rel="noreferrer" className="text-sm text-primary">
                  {t("Open Plex sign-in")}
                </a>
              ) : null}
              {!plexLinked ? (
                <Button
                  variant="outline"
                  type="button"
                  disabled={busy}
                  onClick={() => void linkPlex()}
                >
                  {t("Link Plex account")}
                </Button>
              ) : null}
              {canLinkOIDC ? (
                <Button
                  variant="outline"
                  type="button"
                  disabled={busy}
                  onClick={() => void linkOIDC()}
                >
                  {t("Link {provider} account", { provider: oidcName ?? "" })}
                </Button>
              ) : null}
            </div>
          ) : null
        }
      >
        {!loaded ? (
          <div className="px-5 py-4">
            <LoadingRows rows={2} />
          </div>
        ) : shown.length ? (
          shown.map((method) =>
            method.provider === "invite" ? (
              <SettingsRow key={method.id} title={providerLabel(method.provider)} />
            ) : (
              <SettingsRow
                key={method.id}
                title={method.display || providerLabel(method.provider)}
                description={
                  method.display
                    ? method.provider === "oidc" && oidcName
                      ? oidcName
                      : providerLabel(method.provider)
                    : undefined
                }
                control={
                  method.provider === "oidc" && self && !owner ? (
                    <Button
                      variant="outline"
                      size="sm"
                      type="button"
                      disabled={busy}
                      onClick={() => void unlink(method.id)}
                    >
                      {t("Disconnect")}
                    </Button>
                  ) : undefined
                }
              />
            ),
          )
        ) : (
          <div className="px-5 py-6">
            <EmptyState
              icon={<KeyRound />}
              title={t("No sign-in method yet")}
              description={t("Add a username and password, or link Plex.")}
            />
          </div>
        )}
      </SettingsSection>
      {quickConnect}
      {!owner ? (
        <form onSubmit={update}>
          <SettingsSection
            title={t("Password")}
            description={
              localName
                ? t(
                    "Sign in here and in the Jellyfin and Emby apps with this username and password. Saving replaces your current password.",
                  )
                : t(
                    "Choose a username and password to sign in here and in the Jellyfin and Emby apps.",
                  )
            }
            footer={
              <Button type="submit" disabled={busy || passwordTooShort}>
                {t("Set or reset password")}
              </Button>
            }
          >
            <SettingsRow stacked title={t("Username")}>
              <Input
                id={`${base}-username`}
                aria-label={t("Username")}
                value={localName || username}
                readOnly={Boolean(localName)}
                onChange={(e) => setUsername(e.currentTarget.value)}
                autoComplete="username"
                required
              />
            </SettingsRow>
            <SettingsRow
              stacked
              title={t("New password")}
              description={t("Use at least 12 characters.")}
            >
              <PasswordInput
                id={`${base}-password`}
                aria-label={t("New password")}
                aria-invalid={passwordTooShort || undefined}
                minLength={MIN_LOCAL_PASSWORD}
                value={password}
                onChange={(e) => setPassword(e.currentTarget.value)}
                autoComplete="new-password"
                required
              />
              {passwordTooShort ? (
                <p className="mt-1 text-xs text-danger" role="alert">
                  {t("Password must be at least 12 characters.")}
                </p>
              ) : null}
            </SettingsRow>
          </SettingsSection>
        </form>
      ) : null}
      {showInvite ? (
        <SettingsSection
          title={t("Invite")}
          description={t("Send a link so someone can create their own viewer account.")}
          footer={
            <Button
              variant="outline"
              type="button"
              disabled={busy}
              onClick={() => void makeInvite()}
            >
              {t("Create invite link")}
            </Button>
          }
        >
          {invite ? (
            <SettingsRow title={t("Invite link")} description={invite} />
          ) : (
            <div className="px-5 py-4 text-sm text-fg-muted">
              {t("A new link is created when you ask for one. It is shown here once.")}
            </div>
          )}
        </SettingsSection>
      ) : null}
      {beforeDevices}
      <SettingsSection
        title={t("Devices")}
        description={t("Phones, TVs and browsers that stay signed in.")}
      >
        {!loaded ? (
          <div className="px-5 py-4">
            <LoadingRows rows={2} />
          </div>
        ) : devices.length ? (
          devices.map((device) => {
            const name = deviceName(device);
            const used = lastUsedLabel(device.lastUsed);
            const detail = [device.surface && device.surface !== name ? device.surface : "", used]
              .filter(Boolean)
              .join(" · ");
            return (
              <SettingsRow
                key={device.hash}
                title={name}
                description={detail || undefined}
                control={
                  <Button
                    variant="outline"
                    size="sm"
                    type="button"
                    disabled={busy}
                    onClick={() => void revoke(device.hash)}
                  >
                    {t("Revoke")}
                  </Button>
                }
              />
            );
          })
        ) : (
          <p className="flex items-center gap-2 px-5 py-4 text-sm text-fg-muted">
            <MonitorSmartphone size={16} />
            {t("No devices.")}
          </p>
        )}
      </SettingsSection>
    </div>
  );
}
