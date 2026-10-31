import { t } from "../../i18n";
import { useCallback, useEffect, useRef, useState, type FormEvent } from "react";
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
import { SettingsRow } from "../../components/SettingsRow";

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
  return t("Last used {when}", { when: date.toLocaleString() });
}

export default function SignInMethods({
  base,
  token,
  owner = false,
  showInvite = owner,
}: {
  base: string;
  token: string;
  owner?: boolean;
  /** Invite links are for adding a viewer. The owner's own account page leaves this off. */
  showInvite?: boolean;
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
  async function update(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError("");
    setMessage("");
    try {
      await call("/password", { username, password });
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
    const popup = window.open("", "plex-link", "width=560,height=720");
    try {
      const pin = await call<{ handle: string; authUrl: string }>("/link/plex", {});
      if (cancelled.current) return;
      setPinURL(pin.authUrl);
      if (popup) popup.location.href = pin.authUrl;
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
        description={t("Accounts that can sign in as you.")}
        footer={
          !owner ? (
            <div className="flex flex-wrap items-center gap-3">
              {pinURL ? (
                <a href={pinURL} target="_blank" rel="noreferrer" className="text-sm text-primary">
                  {t("Open Plex sign-in")}
                </a>
              ) : null}
              <Button
                variant="outline"
                type="button"
                disabled={busy}
                onClick={() => void linkPlex()}
              >
                {t("Link Plex account")}
              </Button>
            </div>
          ) : null
        }
      >
        {!loaded ? (
          <div className="px-5 py-4">
            <LoadingRows rows={2} />
          </div>
        ) : methods.length ? (
          methods.map((method) => (
            <SettingsRow
              key={method.id}
              title={method.display || providerLabel(method.provider)}
              description={method.display ? providerLabel(method.provider) : undefined}
            />
          ))
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
      <form onSubmit={update}>
        <SettingsSection
          title={t("Password")}
          description={t("Choose a username and password. Saving replaces the current password.")}
          footer={
            <Button type="submit" disabled={busy}>
              {t("Set or reset password")}
            </Button>
          }
        >
          <SettingsRow stacked title={t("Username")}>
            <Input
              id={`${base}-username`}
              aria-label={t("Username")}
              value={username}
              onChange={(e) => setUsername(e.currentTarget.value)}
              autoComplete="username"
              required
            />
          </SettingsRow>
          <SettingsRow stacked title={t("New password")}>
            <Input
              id={`${base}-password`}
              aria-label={t("New password")}
              type="password"
              value={password}
              onChange={(e) => setPassword(e.currentTarget.value)}
              autoComplete="new-password"
              required
            />
          </SettingsRow>
        </SettingsSection>
      </form>
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
          <div className="px-5 py-6">
            <EmptyState
              icon={<MonitorSmartphone />}
              title={t("No devices.")}
              description={t("Nothing is signed in with this account yet.")}
            />
          </div>
        )}
      </SettingsSection>
    </div>
  );
}
