import { t } from "../../i18n";
import { useEffect, useRef, useState, type FormEvent } from "react";
import { Alert, AlertDescription, Button, Card, Input, Label } from "@braedonsaunders/appkit-ui";
import { PasswordInput, Segmented } from "../../components/fields";
import { EtherLogo } from "../../components/brand";
import { ConnectApps } from "./ConnectApps";
import SignInMethods from "./SignInMethods";

const SESSION_KEY = "ether.viewerPortal";
const CODE_LENGTH = 6;
type Method = "plex" | "local";
type Viewer = { id: number; username?: string; title?: string };
type Approval = { ok: boolean; device?: string; app?: string };

const APP_NAMES: Record<string, string> = {
  plex: "Plex",
  jellyfin: "Jellyfin",
  emby: "Emby",
  silo: "Silo",
  stremio: "Stremio",
};

class PortalError extends Error {
  code: string;
  constructor(code: string, message: string) {
    super(message);
    this.code = code;
  }
}

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
  const text = await res.text();
  const data = text ? (JSON.parse(text) as { error?: { code?: string } }) : {};
  if (!res.ok) {
    throw new PortalError(data.error?.code ?? "", failureMessage(res.status, data.error?.code));
  }
  return (text ? JSON.parse(text) : undefined) as T;
}

function failureMessage(status: number, code: string | undefined): string {
  if (code === "used")
    return t(
      "This code has already been used to sign in a device. If the TV still isn't signed in, start again on the TV to get a new code.",
    );
  if (code === "expired")
    return t("That code has expired. Get a new one on your TV and try again.");
  if (code === "invalid")
    return t("That code isn't valid. Check the code on your TV and try again.");
  if (status === 429) return t("Too many tries. Wait a moment, then enter the code again.");
  if (status === 401) return t("Sign-in failed. Check your credentials.");
  return t("The request could not be completed. Try again.");
}

function normalizeCode(raw: string): string {
  return raw
    .toUpperCase()
    .replace(/[^A-Z0-9]/g, "")
    .slice(0, CODE_LENGTH);
}

function approvalDetail(device: string, app: string): string {
  const known = APP_NAMES[app.toLowerCase()];
  const name = known || app;
  if (name && device && name.toLowerCase() !== device.toLowerCase())
    return t("{app} on {device}", { app: name, device });
  return device || name;
}

/** A short rising chime. Browsers that block autoplay leave it silent. */
function playApprovalSound() {
  const Ctx =
    window.AudioContext ??
    (window as unknown as { webkitAudioContext?: typeof AudioContext }).webkitAudioContext;
  if (!Ctx) return;
  try {
    const ctx = new Ctx();
    const start = ctx.currentTime;
    [523.25, 659.25, 783.99].forEach((freq, i) => {
      const osc = ctx.createOscillator();
      const gain = ctx.createGain();
      const at = start + i * 0.11;
      osc.type = "sine";
      osc.frequency.value = freq;
      gain.gain.setValueAtTime(0.0001, at);
      gain.gain.exponentialRampToValueAtTime(0.07, at + 0.02);
      gain.gain.exponentialRampToValueAtTime(0.0001, at + 0.28);
      osc.connect(gain).connect(ctx.destination);
      osc.start(at);
      osc.stop(at + 0.3);
    });
    void ctx.resume().catch(() => undefined);
    window.setTimeout(() => void ctx.close().catch(() => undefined), 900);
  } catch {
    // Autoplay was blocked, or this browser has no audio engine.
  }
}

export default function Me() {
  const [token, setToken] = useState(() => sessionStorage.getItem(SESSION_KEY) ?? "");
  const [viewer, setViewer] = useState<Viewer | null>(null);
  const [methods, setMethods] = useState<Method[] | null>(null);
  const [method, setMethod] = useState<Method>("plex");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [code, setCode] = useState("");
  const [message, setMessage] = useState("");
  const [approved, setApproved] = useState<Approval | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [pinURL, setPinURL] = useState("");
  const [inviteSecret, setInviteSecret] = useState("");
  const cancelled = useRef(false);
  const codeRef = useRef<HTMLInputElement>(null);
  const submitted = useRef("");

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
    let stop = false;
    fetch("/ether/api/bootstrap")
      .then((res) => (res.ok ? (res.json() as Promise<{ signInMethods?: string[] }>) : null))
      .then((boot) => {
        if (stop) return;
        const offered = (boot?.signInMethods ?? ["local", "plex"]).filter(
          (item): item is Method => item === "local" || item === "plex",
        );
        const next = offered.length ? offered : (["local", "plex"] as Method[]);
        setMethods(next);
        setMethod(next.includes("plex") ? "plex" : "local");
      })
      .catch(() => {
        if (!stop) {
          setMethods(["local", "plex"]);
          setMethod("plex");
        }
      });
    return () => {
      stop = true;
    };
  }, []);

  useEffect(() => {
    if (!token) {
      setViewer(null);
      return;
    }
    let stop = false;
    request<Viewer>("", token)
      .then((v) => {
        if (!stop) setViewer(v);
      })
      .catch(() => {
        if (!stop) {
          sessionStorage.removeItem(SESSION_KEY);
          setToken("");
          setError(t("Your session has expired. Sign in again."));
        }
      });
    return () => {
      stop = true;
    };
  }, [token]);

  useEffect(() => {
    if (viewer && !approved) codeRef.current?.focus();
  }, [viewer, approved]);

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
      setMessage(t("Invite accepted. You can approve a TV now."));
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
    } catch (err) {
      setError(err instanceof Error ? err.message : t("Sign-in failed."));
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

  async function approve(value: string) {
    if (value.length !== CODE_LENGTH || submitted.current === value) return;
    submitted.current = value;
    setError("");
    setMessage("");
    setBusy(true);
    try {
      const result = await request<Approval>("/quickconnect", token, { code: value });
      setApproved(result);
      setCode("");
      playApprovalSound();
    } catch (err) {
      submitted.current = "";
      setError(err instanceof Error ? err.message : t("The request could not be completed."));
    } finally {
      setBusy(false);
    }
  }

  function onCode(raw: string) {
    const next = normalizeCode(raw);
    setCode(next);
    if (next.length === CODE_LENGTH) void approve(next);
  }

  const localOn = methods?.includes("local") ?? false;
  const plexOn = methods?.includes("plex") ?? false;
  const signedIn = Boolean(token && viewer);
  const detail = approved ? approvalDetail(approved.device ?? "", approved.app ?? "") : "";

  return (
    <main className="grid min-h-screen place-items-center bg-bg-subtle p-4">
      <div className="w-full max-w-2xl space-y-6">
        <Card className="mx-auto w-full max-w-md space-y-6 p-7 shadow-lg">
          <EtherLogo className="mx-auto w-40" />
          <div className="space-y-1 text-center">
            <h1 className="text-xl font-semibold text-fg">{t("Approve a TV")}</h1>
            {approved ? null : (
              <p className="text-sm text-fg-muted">
                {signedIn
                  ? t("Enter the code shown on your TV.")
                  : t("Sign in, then enter the code shown on your TV.")}
              </p>
            )}
          </div>
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
            <Button
              type="button"
              className="w-full"
              disabled={busy}
              onClick={() => void redeemInvite()}
            >
              {t("Accept invite")}
            </Button>
          ) : null}
          {!token ? (
            methods == null ? (
              <p role="status" className="text-center text-sm text-fg-muted">
                {t("Loading…")}
              </p>
            ) : !localOn && !plexOn ? (
              <p className="text-sm text-fg-muted">
                {t("No sign-in method is available. Contact the server owner.")}
              </p>
            ) : (
              <div className="space-y-5">
                {localOn && plexOn ? (
                  <Segmented
                    ariaLabel={t("Sign-in method")}
                    className="flex w-full [&>button]:min-w-0 [&>button]:flex-1"
                    value={method}
                    onChange={setMethod}
                    options={[
                      { value: "plex", label: t("Sign in with Plex") },
                      { value: "local", label: t("Username & password") },
                    ]}
                  />
                ) : null}
                {method === "local" && localOn ? (
                  <form className="space-y-4" onSubmit={(e) => void login(e)}>
                    <div className="space-y-2">
                      <Label htmlFor="me-username">{t("Username")}</Label>
                      <Input
                        id="me-username"
                        autoComplete="username"
                        value={username}
                        onChange={(e) => setUsername(e.currentTarget.value)}
                        required
                      />
                    </div>
                    <div className="space-y-2">
                      <Label htmlFor="me-password">{t("Password")}</Label>
                      <PasswordInput
                        id="me-password"
                        autoComplete="current-password"
                        value={password}
                        onChange={(e) => setPassword(e.currentTarget.value)}
                        required
                      />
                    </div>
                    <Button type="submit" className="w-full" disabled={busy}>
                      {t("Sign in")}
                    </Button>
                  </form>
                ) : null}
                {method === "plex" && plexOn ? (
                  <div className="space-y-3">
                    <Button
                      type="button"
                      className="w-full"
                      disabled={busy}
                      onClick={() => void plexSignIn()}
                    >
                      {t("Sign in with Plex")}
                    </Button>
                    {pinURL ? (
                      <p className="text-center text-sm">
                        <a href={pinURL} target="_blank" rel="noreferrer">
                          {t("Open Plex sign-in")}
                        </a>
                      </p>
                    ) : null}
                  </div>
                ) : null}
              </div>
            )
          ) : viewer ? (
            approved ? (
              <div role="status" className="space-y-5 text-center">
                <div className="ether-approve text-primary" aria-hidden="true">
                  <span className="ether-approve-ring" />
                  <span className="ether-approve-burst">
                    {Array.from({ length: 8 }, (_, i) => (
                      <span key={i} style={{ ["--ether-burst" as string]: `${i * 45}deg` }} />
                    ))}
                  </span>
                  <svg viewBox="0 0 160 160" className="ether-approve-pop h-40 w-40">
                    <circle cx="80" cy="80" r="68" className="ether-approve-glow fill-primary/15" />
                    <path
                      d="M46 84 L72 110 L116 54"
                      className="ether-approve-check fill-none stroke-current"
                      strokeWidth="10"
                      strokeLinecap="round"
                      strokeLinejoin="round"
                    />
                  </svg>
                </div>
                <div className="space-y-2">
                  <h2 className="text-4xl font-semibold tracking-tight text-fg">
                    {t("Device approved")}
                  </h2>
                  {detail ? <p className="text-lg text-fg">{detail}</p> : null}
                </div>
                <Button
                  type="button"
                  variant="outline"
                  className="w-full"
                  onClick={() => {
                    submitted.current = "";
                    setApproved(null);
                    setCode("");
                  }}
                >
                  {t("Approve another device")}
                </Button>
              </div>
            ) : (
              <form
                className="space-y-4"
                onSubmit={(e) => {
                  e.preventDefault();
                  void approve(code);
                }}
              >
                <p className="text-center text-sm text-fg-muted">
                  {viewer.title || viewer.username}
                </p>
                <div className="space-y-2">
                  <Label htmlFor="me-code" className="sr-only">
                    {t("Enter the code shown on your TV")}
                  </Label>
                  <Input
                    ref={codeRef}
                    id="me-code"
                    value={code}
                    onChange={(e) => onCode(e.currentTarget.value)}
                    autoFocus
                    autoComplete="one-time-code"
                    autoCapitalize="characters"
                    spellCheck={false}
                    inputMode="text"
                    aria-label={t("Enter the code shown on your TV")}
                    className="h-16 text-center font-mono text-3xl tracking-[0.3em] uppercase"
                  />
                </div>
                <Button
                  type="submit"
                  className="w-full"
                  disabled={busy || code.length !== CODE_LENGTH}
                >
                  {t("Approve device")}
                </Button>
              </form>
            )
          ) : (
            <p role="status">{t("Loading…")}</p>
          )}
          {token ? (
            <Button
              variant="ghost"
              type="button"
              className="w-full"
              onClick={() => {
                sessionStorage.removeItem(SESSION_KEY);
                setToken("");
                setViewer(null);
                setApproved(null);
                setMessage("");
                setCode("");
              }}
            >
              {t("Sign out")}
            </Button>
          ) : null}
        </Card>
        {signedIn ? (
          <>
            <ConnectApps base="/me" token={token} />
            <SignInMethods base="/me" token={token} />
          </>
        ) : null}
      </div>
    </main>
  );
}
