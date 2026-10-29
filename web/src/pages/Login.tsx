import { Alert, AlertDescription, Button, Card, Input, Label } from "@braedonsaunders/appkit-ui";
import { ChevronRight, KeyRound } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { EtherLogo } from "../components/brand";
import { t, tx } from "../i18n";
import { LanguagePicker } from "../i18n/react";
import { api, ApiError, errorMessage, getToken, setToken } from "../lib/api";
import type { Bootstrap } from "../lib/types";

const SIGN_IN_TIMEOUT_MS = 10 * 60 * 1000;
const POLL_MS = 2000;

/**
 * Owner sign-in is required on every connection, including localhost.
 */
export default function Login({ boot, onReady }: { boot: Bootstrap; onReady: () => void }) {
  const [error, setError] = useState<unknown>(null);
  const [pending, setPending] = useState<{ code: string; authUrl: string } | null>(null);
  const [busy, setBusy] = useState(false);
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const localSignIn = async (e: React.FormEvent) => {
    e.preventDefault();
    setError(null);
    setBusy(true);
    try {
      const result = await api.localSignIn(username, password);
      if (!cancelled.current) {
        setToken(result.token);
        onReady();
      }
    } catch (err) {
      if (!cancelled.current) setError(err);
    } finally {
      if (!cancelled.current) setBusy(false);
    }
  };
  const cancelled = useRef(false);
  const popupRef = useRef<Window | null>(null);
  useEffect(() => {
    cancelled.current = false;
    return () => {
      cancelled.current = true;
      popupRef.current?.close();
      popupRef.current = null;
    };
  }, []);

  const signIn = async () => {
    setError(null);
    setBusy(true);
    // Open the window inside the click so popup blockers allow it.
    const popup = window.open("", "plex-sign-in", "width=560,height=720");
    popupRef.current = popup;
    try {
      const pin = await api.signInStart();
      if (cancelled.current) return;
      setPending({ code: pin.code, authUrl: pin.authUrl });
      if (popup) popup.location.href = pin.authUrl;
      const deadline = Date.now() + SIGN_IN_TIMEOUT_MS;
      while (!cancelled.current && Date.now() < deadline) {
        await new Promise((r) => setTimeout(r, POLL_MS));
        if (cancelled.current) return;
        const res = await api.signInPoll(pin.handle);
        if (cancelled.current) return;
        if (res.done && res.token) {
          setToken(res.token);
          popup?.close();
          onReady();
          return;
        }
      }
      if (!cancelled.current) setError(new Error(t("Sign-in timed out. Try again.")));
    } catch (err) {
      if (cancelled.current) return;
      popup?.close();
      setError(
        err instanceof ApiError && err.code === "not_owner"
          ? new Error(
              t(
                "That Plex account doesn't own this server. Sign in with the account that set Ether up.",
              ),
            )
          : err,
      );
    } finally {
      popup?.close();
      if (popupRef.current === popup) popupRef.current = null;
      if (!cancelled.current) {
        setBusy(false);
        setPending(null);
      }
    }
  };

  return (
    <main className="relative grid min-h-screen place-items-center bg-bg-subtle p-4">
      <div className="absolute right-4 top-4">
        <LanguagePicker side="bottom" className="h-8 px-2 text-xs" />
      </div>
      <Card className="reveal w-full max-w-sm p-7 shadow-lg">
        <div className="mb-6 flex flex-col items-center text-center">
          <EtherLogo className="w-48" />
          <h1 className="sr-only">{boot.serverName || "Ether"}</h1>
          <p className="mt-4 text-sm text-fg-muted">{t("Sign in to the owner account.")}</p>
        </div>
        {error ? (
          <Alert variant="destructive" className="mb-4" role="alert">
            <AlertDescription>{errorMessage(error)}</AlertDescription>
          </Alert>
        ) : null}
        <Button
          type="button"
          size="lg"
          className="w-full"
          onClick={() => void signIn()}
          disabled={busy}
        >
          {busy ? t("Waiting for Plex…") : t("Sign in with Plex")}
        </Button>
        {pending ? (
          <p className="mt-3 text-center text-xs text-fg-muted">
            {tx("Finish signing in in the Plex window. No window? {link}.", {
              link: (
                <a
                  className="font-medium text-primary underline-offset-4 hover:underline"
                  href={pending.authUrl}
                  target="_blank"
                  rel="noreferrer"
                >
                  {t("Open the Plex sign-in page")}
                </a>
              ),
            })}
          </p>
        ) : null}
        <form onSubmit={(e) => void localSignIn(e)} className="mt-5 space-y-3">
          <Label htmlFor="local-username">{t("Username")}</Label>
          <Input
            id="local-username"
            autoComplete="username"
            value={username}
            onChange={(e) => setUsername(e.currentTarget.value)}
          />
          <Label htmlFor="local-password">{t("Password")}</Label>
          <Input
            id="local-password"
            type="password"
            autoComplete="current-password"
            value={password}
            onChange={(e) => setPassword(e.currentTarget.value)}
          />
          <Button
            type="submit"
            variant="outline"
            className="w-full"
            disabled={busy || !username || !password}
          >
            {t("Sign in")}
          </Button>
        </form>
        <TokenFallback onReady={onReady} />
        <p className="mt-6 text-center text-xs text-fg-subtle">
          {t("Only the owner can access this console.")}
        </p>
      </Card>
    </main>
  );
}

function TokenFallback({ onReady }: { onReady: () => void }) {
  const [value, setValue] = useState(getToken());
  const [error, setError] = useState<unknown>(null);
  const [checking, setChecking] = useState(false);
  const cancelled = useRef(false);
  useEffect(() => {
    cancelled.current = false;
    return () => {
      cancelled.current = true;
    };
  }, []);
  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setChecking(true);
    setError(null);
    const token = value.trim();
    setToken(token);
    try {
      await api.setupStatus(); // cheap authenticated probe
      if (cancelled.current || getToken() !== token) return;
      onReady();
    } catch (err) {
      if (cancelled.current || getToken() !== token) return;
      setToken("");
      setError(
        err instanceof ApiError && err.status === 401
          ? new Error(t("That token was not accepted as the owner token."))
          : err,
      );
    } finally {
      if (!cancelled.current) setChecking(false);
    }
  };
  return (
    <details className="group mt-5 rounded-lg border border-border-subtle px-3 py-2">
      <summary className="flex cursor-pointer list-none items-center gap-1.5 text-xs font-medium text-fg-muted hover:text-fg">
        <ChevronRight size={14} className="transition-transform group-open:rotate-90" aria-hidden />
        {t("Use a Plex token instead")}
      </summary>
      <form onSubmit={submit} className="mt-3 space-y-3">
        {error ? (
          <p className="text-xs text-danger" role="alert">
            {errorMessage(error)}
          </p>
        ) : null}
        <div className="space-y-1.5">
          <Label htmlFor="owner-token" className="text-xs">
            {t("Owner token (X-Plex-Token)")}
          </Label>
          <Input
            id="owner-token"
            type="password"
            value={value}
            onChange={(e) => setValue(e.currentTarget.value)}
            autoComplete="off"
          />
        </div>
        <Button
          type="submit"
          variant="outline"
          size="sm"
          className="w-full"
          disabled={checking || !value.trim()}
        >
          <KeyRound size={14} /> {checking ? t("Checking…") : t("Unlock")}
        </Button>
      </form>
    </details>
  );
}
