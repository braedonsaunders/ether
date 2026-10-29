import { t } from "../../i18n";
import { useCallback, useEffect, useRef, useState, type FormEvent } from "react";
import { Button, Input, Label } from "@braedonsaunders/appkit-ui";

type Method = { id: number; provider: string; display: string };
type Device = {
  hash: string;
  surface: string;
  client: string;
  product: string;
  version: string;
  lastUsed: string | number;
};

export default function SignInMethods({
  base,
  token,
  owner = false,
}: {
  base: string;
  token: string;
  owner?: boolean;
}) {
  const [methods, setMethods] = useState<Method[]>([]);
  const [devices, setDevices] = useState<Device[]>([]);
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
    load().catch(() => {
      if (!cancelled) setError(t("Sign-in methods and devices could not be loaded."));
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
        <p role="alert" className="text-sm text-danger">
          {error}
        </p>
      ) : null}
      {message ? (
        <p role="status" className="text-sm text-fg">
          {message}
        </p>
      ) : null}
      <section className="space-y-3">
        <h2 className="text-lg font-semibold">{t("Sign-in methods")}</h2>
        <ul>
          {methods.map((m) => (
            <li key={m.id} className="text-sm">
              {m.provider} · {m.display}
            </li>
          ))}
        </ul>
        {!owner ? (
          <div>
            <Button variant="outline" type="button" disabled={busy} onClick={() => void linkPlex()}>
              {t("Link Plex account")}
            </Button>
            {pinURL ? (
              <p>
                <a href={pinURL} target="_blank" rel="noreferrer">
                  {t("Open Plex sign-in")}
                </a>
              </p>
            ) : null}
          </div>
        ) : null}
        <form className="space-y-3" onSubmit={update}>
          <div>
            <Label htmlFor={`${base}-username`}>{t("Username")}</Label>
            <Input
              id={`${base}-username`}
              value={username}
              onChange={(e) => setUsername(e.currentTarget.value)}
              autoComplete="username"
              required
            />
          </div>
          <div>
            <Label htmlFor={`${base}-password`}>{t("New password")}</Label>
            <Input
              id={`${base}-password`}
              type="password"
              value={password}
              onChange={(e) => setPassword(e.currentTarget.value)}
              autoComplete="new-password"
              required
            />
          </div>
          <Button type="submit" disabled={busy}>
            {t("Set or reset password")}
          </Button>
        </form>
        {owner ? (
          <div>
            <Button
              variant="outline"
              type="button"
              disabled={busy}
              onClick={() => void makeInvite()}
            >
              {t("Create invite link")}
            </Button>
            {invite ? <p className="break-all text-sm">{invite}</p> : null}
          </div>
        ) : null}
      </section>
      <section className="space-y-3">
        <h2 className="text-lg font-semibold">{t("Devices")}</h2>
        {devices.length ? (
          <ul className="space-y-2">
            {devices.map((d) => (
              <li key={d.hash} className="flex items-center justify-between gap-2">
                <span className="text-sm">
                  {d.surface} · {d.client || d.product} · {new Date(d.lastUsed).toLocaleString()}
                </span>
                <Button
                  variant="outline"
                  type="button"
                  disabled={busy}
                  onClick={() => void revoke(d.hash)}
                >
                  {t("Revoke")}
                </Button>
              </li>
            ))}
          </ul>
        ) : (
          <p className="text-sm text-fg-muted">{t("No devices.")}</p>
        )}
      </section>
    </div>
  );
}
