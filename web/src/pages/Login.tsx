import { Alert, AlertDescription, Button, Card, Input, Label } from "@braedonsaunders/appkit-ui";
import { ChevronRight, KeyRound } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { EtherLogo } from "../components/brand";
import { PasswordInput } from "../components/fields";
import { t, tx } from "../i18n";
import { LanguagePicker } from "../i18n/react";
import { api, ApiError, errorMessage, setToken } from "../lib/api";
import type { Bootstrap } from "../lib/types";

const SIGN_IN_TIMEOUT_MS = 10 * 60 * 1000;
const POLL_MS = 2000;

/**
 * Owner sign-in is required on every connection, including localhost.
 */
export default function Login({ boot, onReady }: { boot: Bootstrap; onReady: () => void }) {
  // Offer only the methods the server explicitly advertises.
  const methods = boot.signInMethods ?? [];
  const localEnabled = methods.includes("local");
  const plexEnabled = methods.includes("plex");
  const oidcEnabled = methods.includes("oidc");
  const oidcName = boot.oidcName?.trim() || "SSO";
  const [error, setError] = useState<unknown>(null);
  const [pending, setPending] = useState<{
    kind: "plex" | "oidc";
    authUrl: string;
  } | null>(null);
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

  const signIn = async (kind: "plex" | "oidc") => {
    setError(null);
    setBusy(true);
    // Open the window inside the click so popup blockers allow it.
    const popup = window.open("", `${kind}-sign-in`, "width=560,height=720");
    popupRef.current = popup;
    const poll = kind === "plex" ? api.signInPoll : api.oidcSignInPoll;
    try {
      const pin = await (kind === "plex" ? api.signInStart() : api.oidcSignInStart());
      if (cancelled.current) return;
      setPending({ kind, authUrl: pin.authUrl });
      if (popup) {
        // The provider's page must not be able to navigate this tab.
        popup.opener = null;
        popup.location.href = pin.authUrl;
      }
      const deadline = Date.now() + SIGN_IN_TIMEOUT_MS;
      while (!cancelled.current && Date.now() < deadline) {
        await new Promise((r) => setTimeout(r, POLL_MS));
        if (cancelled.current) return;
        const res = await poll(pin.handle);
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
      const notOwner = err instanceof ApiError && err.code === "not_owner";
      const notLinked = err instanceof ApiError && err.code === "not_linked";
      setError(
        notOwner && kind === "plex"
          ? new Error(
              t(
                "That Plex account doesn't own this server. Sign in with the account that set Ether up.",
              ),
            )
          : notOwner
            ? new Error(
                t("That {provider} account doesn't own this server.", { provider: oidcName }),
              )
            : notLinked
              ? new Error(
                  t(
                    "That {provider} account isn't linked yet. Sign in another way, then link it under Sign-in methods at /ether/me.",
                    { provider: oidcName },
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
          <p className="mt-4 text-sm text-fg-muted">{t("Sign in to Ether")}</p>
        </div>
        {error ? (
          <Alert variant="destructive" className="mb-4" role="alert">
            <AlertDescription>{errorMessage(error)}</AlertDescription>
          </Alert>
        ) : null}
        {localEnabled ? (
          <form onSubmit={(e) => void localSignIn(e)} className="mt-5 space-y-3">
            <Label htmlFor="local-username">{t("Username")}</Label>
            <Input
              id="local-username"
              autoComplete="username"
              value={username}
              onChange={(e) => setUsername(e.currentTarget.value)}
            />
            <Label htmlFor="local-password">{t("Password")}</Label>
            <PasswordInput
              id="local-password"
              autoComplete="current-password"
              value={password}
              onChange={(e) => setPassword(e.currentTarget.value)}
            />
            <Button
              type="submit"
              size="lg"
              className="w-full"
              disabled={busy || !username || !password}
            >
              {t("Sign in")}
            </Button>
          </form>
        ) : null}
        {plexEnabled ? (
          <>
            <Button
              type="button"
              size="lg"
              variant={localEnabled ? "outline" : undefined}
              className="mt-5 w-full"
              onClick={() => void signIn("plex")}
              disabled={busy}
            >
              {pending?.kind === "plex" ? t("Waiting for Plex…") : t("Sign in with Plex")}
            </Button>
            {pending?.kind === "plex" ? (
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
            <TokenFallback onReady={onReady} />
          </>
        ) : null}
        {oidcEnabled ? (
          <>
            <Button
              type="button"
              size="lg"
              variant={localEnabled || plexEnabled ? "outline" : undefined}
              className="mt-5 w-full"
              onClick={() => void signIn("oidc")}
              disabled={busy}
            >
              {pending?.kind === "oidc"
                ? t("Waiting for {provider}…", { provider: oidcName })
                : t("Sign in with {provider}", { provider: oidcName })}
            </Button>
            {pending?.kind === "oidc" ? (
              <p className="mt-3 text-center text-xs text-fg-muted">
                {tx("Finish signing in in the {provider} window. No window? {link}.", {
                  provider: oidcName,
                  link: (
                    <a
                      className="font-medium text-primary underline-offset-4 hover:underline"
                      href={pending.authUrl}
                      target="_blank"
                      rel="noreferrer"
                    >
                      {t("Open the sign-in page")}
                    </a>
                  ),
                })}
              </p>
            ) : null}
          </>
        ) : null}
        {!localEnabled && !plexEnabled && !oidcEnabled ? (
          <p className="mt-5 text-sm text-fg-muted">
            {t("No sign-in method is available. Contact the server owner.")}
          </p>
        ) : null}
        <p className="mt-6 text-center text-xs text-fg-subtle">
          {t("Only the owner can access this console.")}
        </p>
      </Card>
    </main>
  );
}

function TokenFallback({ onReady }: { onReady: () => void }) {
  const [value, setValue] = useState("");
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
    try {
      // The pasted Plex token is exchanged for a console session and never
      // stored in the browser.
      const { token: session } = await api.createSession(token);
      if (cancelled.current) return;
      setToken(session);
      onReady();
    } catch (err) {
      if (cancelled.current) return;
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
          <PasswordInput
            id="owner-token"
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
