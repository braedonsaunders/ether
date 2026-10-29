import { t } from "../../i18n";
import { useCallback, useEffect, useRef, useState, type FormEvent } from "react";
import { Alert, AlertDescription, Button, Card, Input, Label } from "@braedonsaunders/appkit-ui";
import SignInMethods from "./SignInMethods";
import { FavoritesMirror } from "./FavoritesMirror";
import { FavoriteEpisodeAlerts } from "./FavoriteEpisodeAlerts";
import { ViewerPush } from "./ViewerPush";
import { EtherLogo } from "../../components/brand";

const SESSION_KEY = "ether.viewerPortal";
type Viewer = { id: number; username?: string; title?: string };

async function request<T>(
  path: string,
  token: string,
  body?: unknown,
  method?: "POST" | "PUT" | "DELETE",
): Promise<T> {
  const res = await fetch(`/ether/api/me${path}`, {
    method: method ?? (body === undefined ? "GET" : "POST"),
    headers: {
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
      ...(body === undefined ? {} : { "Content-Type": "application/json", "X-Ether-Request": "1" }),
    },
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
  });
  if (!res.ok)
    throw new Error(
      res.status === 401
        ? t("Sign-in failed. Check your credentials.")
        : t("The request could not be completed. Try again."),
    );
  return res.status === 204 ? (undefined as T) : (res.json() as Promise<T>);
}

export default function Me() {
  const [token, setToken] = useState(() => sessionStorage.getItem(SESSION_KEY) ?? "");
  const [viewer, setViewer] = useState<Viewer | null>(null);
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [code, setCode] = useState("");
  const [message, setMessage] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [pinURL, setPinURL] = useState("");
  const [inviteSecret, setInviteSecret] = useState("");
  const cancelled = useRef(false);
  useEffect(() => {
    cancelled.current = false;
    return () => {
      cancelled.current = true;
    };
  }, []);

  useEffect(() => {
    const params = new URLSearchParams(window.location.hash.slice(1));
    const secret = params.get("invite");
    if (secret) {
      setInviteSecret(secret);
      window.history.replaceState(
        window.history.state,
        "",
        window.location.pathname + window.location.search,
      );
    }
  }, []);

  useEffect(() => {
    if (!token) {
      setViewer(null);
      return;
    }
    let cancelled = false;
    request<Viewer>("", token)
      .then((v) => {
        if (!cancelled) setViewer(v);
      })
      .catch(() => {
        if (!cancelled) {
          sessionStorage.removeItem(SESSION_KEY);
          setToken("");
          setError(t("Your session has expired. Sign in again."));
        }
      });
    return () => {
      cancelled = true;
    };
  }, [token]);

  const loadFavorites = useCallback(
    () => request<{ enabled: boolean }>("/favorites-mirror", token),
    [token],
  );
  const saveFavorites = useCallback(
    (enabled: boolean) => request("/favorites-mirror", token, { enabled }),
    [token],
  );
  const loadAlerts = useCallback(
    () => request<{ enabled: boolean }>("/favorite-episode-alert", token),
    [token],
  );
  const saveAlerts = useCallback(
    (enabled: boolean) => request("/favorite-episode-alert", token, { enabled }),
    [token],
  );
  const getPushKey = useCallback(
    () => request<{ publicKey: string }>("/alerts/push/key", token),
    [token],
  );
  const pushStatus = useCallback(
    (endpoint: string) =>
      request<{ registered: boolean }>("/alerts/push/status", token, { endpoint }),
    [token],
  );
  const pushSubscribe = useCallback(
    (subscription: PushSubscriptionJSON) =>
      request(
        "/alerts/push/subscription",
        token,
        { endpoint: subscription.endpoint, keys: subscription.keys },
        "PUT",
      ),
    [token],
  );
  const pushRemove = useCallback(
    (endpoint: string) => request("/alerts/push/subscription", token, { endpoint }, "DELETE"),
    [token],
  );

  async function redeemInvite() {
    setBusy(true);
    setError("");
    try {
      const result = await request<{ token: string }>("/invite/redeem", "", {
        secret: inviteSecret,
      });
      sessionStorage.setItem(SESSION_KEY, result.token);
      setToken(result.token);
      setInviteSecret("");
      setMessage(t("Invite accepted. Set a password to sign in again."));
    } catch {
      setError(t("This invite could not be accepted."));
    } finally {
      setBusy(false);
    }
  }

  async function login(e: FormEvent) {
    e.preventDefault();
    setError("");
    setBusy(true);
    try {
      const result = await request<{ token: string }>("/login", "", { username, password });
      sessionStorage.setItem(SESSION_KEY, result.token);
      setToken(result.token);
      setPassword("");
    } catch (e) {
      setError(e instanceof Error ? e.message : t("Sign-in failed."));
    } finally {
      setBusy(false);
    }
  }
  async function plexSignIn() {
    setError("");
    setBusy(true);
    const popup = window.open("", "plex-viewer-sign-in", "width=560,height=720");
    try {
      const pin = await request<{ handle: string; authUrl: string }>("/pin", "", {});
      setPinURL(pin.authUrl);
      if (popup) popup.location.href = pin.authUrl;
      const deadline = Date.now() + 10 * 60 * 1000;
      while (!cancelled.current && Date.now() < deadline) {
        await new Promise((resolve) => setTimeout(resolve, 2000));
        if (cancelled.current) return;
        const result = await request<{ done: boolean; token?: string }>(
          `/pin/${encodeURIComponent(pin.handle)}`,
          "",
        );
        if (result.done && result.token) {
          sessionStorage.setItem(SESSION_KEY, result.token);
          setToken(result.token);
          return;
        }
      }
      if (!cancelled.current) setError(t("Sign-in timed out. Try again."));
    } catch {
      if (!cancelled.current) setError(t("Sign-in failed. Try again."));
    } finally {
      popup?.close();
      if (!cancelled.current) {
        setBusy(false);
        setPinURL("");
      }
    }
  }
  async function approve(e: FormEvent) {
    e.preventDefault();
    setError("");
    setMessage("");
    setBusy(true);
    try {
      await request("/quickconnect", token, { code });
      setCode("");
      setMessage(t("Device approved."));
    } catch (e) {
      setError(e instanceof Error ? e.message : t("The request could not be completed."));
    } finally {
      setBusy(false);
    }
  }
  return (
    <main className="grid min-h-screen place-items-center bg-bg-subtle p-4">
      <Card className="w-full max-w-sm space-y-5 p-7 shadow-lg">
        <EtherLogo className="mx-auto w-40" />
        <h1 className="text-xl font-semibold text-fg">{t("Viewer portal")}</h1>
        {error ? (
          <Alert variant="destructive" role="alert">
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        ) : null}
        {message ? (
          <p role="status" className="text-sm text-fg">
            {message}
          </p>
        ) : null}
        {inviteSecret ? (
          <Button type="button" disabled={busy} onClick={() => void redeemInvite()}>
            {t("Accept invite")}
          </Button>
        ) : null}
        {!token ? (
          <form className="space-y-4" onSubmit={login}>
            <div>
              <Label htmlFor="me-username">{t("Username")}</Label>
              <Input
                id="me-username"
                autoComplete="username"
                value={username}
                onChange={(e) => setUsername(e.currentTarget.value)}
                required
              />
            </div>
            <div>
              <Label htmlFor="me-password">{t("Password")}</Label>
              <Input
                id="me-password"
                type="password"
                autoComplete="current-password"
                value={password}
                onChange={(e) => setPassword(e.currentTarget.value)}
                required
              />
            </div>
            <Button type="submit" disabled={busy}>
              {t("Sign in")}
            </Button>
            <Button
              variant="outline"
              type="button"
              disabled={busy}
              onClick={() => void plexSignIn()}
            >
              {t("Sign in with Plex")}
            </Button>
            {pinURL ? (
              <p>
                <a href={pinURL} target="_blank" rel="noreferrer">
                  {t("Open Plex sign-in")}
                </a>
              </p>
            ) : null}
          </form>
        ) : viewer ? (
          <>
            <p className="text-sm text-fg-muted">{viewer.title || viewer.username}</p>
            <form className="space-y-3" onSubmit={approve}>
              <Label htmlFor="me-code">{t("Enter the code shown on your TV")}</Label>
              <Input
                id="me-code"
                value={code}
                onChange={(e) => setCode(e.currentTarget.value)}
                required
                autoComplete="off"
              />
              <Button type="submit" disabled={busy}>
                {t("Approve device")}
              </Button>
            </form>
            <FavoritesMirror load={loadFavorites} save={saveFavorites} />
            <FavoriteEpisodeAlerts load={loadAlerts} save={saveAlerts} />
            <ViewerPush
              getKey={getPushKey}
              status={pushStatus}
              subscribe={pushSubscribe}
              remove={pushRemove}
            />
            <SignInMethods base="/me" token={token} />
            <Button
              variant="outline"
              type="button"
              onClick={() => {
                sessionStorage.removeItem(SESSION_KEY);
                setToken("");
                setMessage("");
              }}
            >
              {t("Sign out")}
            </Button>
          </>
        ) : (
          <p role="status">{t("Loading…")}</p>
        )}
      </Card>
    </main>
  );
}
