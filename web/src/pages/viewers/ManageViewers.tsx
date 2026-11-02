import { t, tx } from "../../i18n";
import { useEffect, useState, type FormEvent } from "react";
import { Button, Input, Label, toast } from "@braedonsaunders/appkit-ui";
import { Copy, KeyRound, Link2, MessageSquareText, MonitorSmartphone } from "lucide-react";
import { useAppOptional } from "../../app-context";
import { Drawer } from "../../components/Drawer";
import { PasswordInput, Segmented } from "../../components/fields";
import { apiFetch, errorMessage } from "../../lib/api";
import { MIN_LOCAL_PASSWORD } from "../me/SignInMethods";
import { usableAddresses, type ReachabilityStatus } from "../apps/reachability";
import { PLEX_ACCESS_URL } from "./ViewerStatus";

type Method = "local" | "invite" | "plex";

type FrontendInfo = {
  descriptor: { id: string };
  enabled: boolean;
  status: ReachabilityStatus;
};

const APP_NAMES: Record<string, string> = {
  plex: "Plex",
  jellyfin: "Jellyfin",
  emby: "Emby",
  silo: "Silo",
  stremio: "Stremio",
  webdav: "WebDAV",
  m3u: "M3U",
};

function portalUrl(): string {
  return typeof window !== "undefined" && window.location?.origin
    ? `${window.location.origin}/ether/me`
    : "/ether/me";
}

/** Suggest a username from the name the owner typed ("Sam Rivera" → "sam"). */
function suggestUsername(name: string): string {
  return name
    .trim()
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "");
}

/**
 * Add a viewer in a drawer: name, how they sign in, then how they connect.
 * Plex accounts are shared on plex.tv; Ether never creates one.
 */
export function AddViewerDrawer({
  open,
  onClose,
  onCreated,
}: {
  open: boolean;
  onClose: () => void;
  onCreated: () => void;
}) {
  const plexConnected = useAppOptional()?.boot.plexEnabled !== false;
  const [step, setStep] = useState(1);
  const [name, setName] = useState("");
  const [method, setMethod] = useState<Method>("local");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [link, setLink] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [frontends, setFrontends] = useState<FrontendInfo[]>([]);

  useEffect(() => {
    if (!open) return;
    let live = true;
    apiFetch<FrontendInfo[]>("/frontends")
      .then((list) => {
        if (live && Array.isArray(list)) setFrontends(list);
      })
      .catch(() => {
        if (live) setFrontends([]);
      });
    return () => {
      live = false;
    };
  }, [open]);

  const enabledApps = frontends.filter((f) => f.enabled && APP_NAMES[f.descriptor.id]);
  const addressFor = (id: string) => {
    const info = enabledApps.find((f) => f.descriptor.id === id);
    return usableAddresses(info?.status)[0] ?? info?.status?.connectUrls?.[0] ?? "";
  };

  function close() {
    setStep(1);
    setName("");
    setMethod("local");
    setUsername("");
    setPassword("");
    setLink("");
    setError("");
    onClose();
  }

  async function create(e: FormEvent) {
    e.preventDefault();
    if (step === 1) {
      if (!username) setUsername(suggestUsername(name));
      setStep(2);
      return;
    }
    if (step === 3) return;
    if (method === "plex") {
      setStep(3);
      return;
    }
    if (method === "local" && password.length < MIN_LOCAL_PASSWORD) {
      setError(t("Password must be at least 12 characters."));
      return;
    }
    setBusy(true);
    setError("");
    try {
      if (method === "local") {
        await apiFetch("/users/local", {
          method: "POST",
          body: JSON.stringify({ username, password }),
        });
        toast.success(t("Viewer added"));
      } else {
        const result = await apiFetch<{ url: string }>("/users/invite", {
          method: "POST",
          body: JSON.stringify({ display: name }),
        });
        setLink(new URL(result.url, window.location.origin).href);
      }
      onCreated();
      setStep(3);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  }

  /** One "How to connect" card per enabled app. */
  const connectCards: { id: string; title: string; lines: string[] }[] = [];
  for (const app of enabledApps) {
    const id = app.descriptor.id;
    const title = APP_NAMES[id]!;
    if (id === "plex") {
      connectCards.push({
        id,
        title,
        lines:
          method === "plex"
            ? [
                t(
                  "Share your server with their Plex account on plex.tv. They appear here automatically within a few minutes.",
                ),
              ]
            : [
                t("Their Plex account must be shared with this server on plex.tv."),
                t("Local viewers can't use Plex apps unless they link a Plex account."),
              ],
      });
    } else if (id === "jellyfin" || id === "emby" || id === "silo") {
      const address = addressFor(id);
      const lines: string[] = [];
      if (address) lines.push(t("Server address: {address}", { address }));
      if (method === "local" && username) {
        lines.push(t("They sign in as {username}.", { username }));
      }
      lines.push(
        t("Or use Quick Connect: they approve the code from the app at {url}.", {
          url: portalUrl(),
        }),
      );
      lines.push(
        t(
          "Good apps to start with: Infuse, Swiftfin, Findroid, Streamyfin and Jellyfin for Android TV.",
        ),
      );
      connectCards.push({ id, title, lines });
    } else {
      connectCards.push({
        id,
        title,
        lines: [t("They get their link at {url}.", { url: portalUrl() })],
      });
    }
  }
  if (connectCards.length === 0) {
    connectCards.push({
      id: "portal",
      title: t("On the web"),
      lines: [t("They get their link at {url}.", { url: portalUrl() })],
    });
  }

  function messageText(): string {
    const lines: string[] = [t("Hi {name}, you can watch on my Ether server.", { name })];
    if (method === "invite" && link) {
      lines.push(t("Set up your account here: {link}", { link }));
    }
    if (method === "local") {
      if (username) lines.push(t("Your username is {username}.", { username }));
      if (password) lines.push(t("Your password is {password}.", { password }));
    }
    if (method === "plex") {
      lines.push(t("I've shared my server with your Plex account. Open the Plex app to watch."));
    }
    for (const card of connectCards) {
      lines.push(`${card.title}: ${card.lines.join(" ")}`);
    }
    return lines.join("\n");
  }

  const passwordTooShort =
    method === "local" && password !== "" && password.length < MIN_LOCAL_PASSWORD;

  return (
    <Drawer open={open} onClose={close} title={t("Add viewer")}>
      <form className="space-y-5" onSubmit={create}>
        <p className="text-xs font-medium text-fg-muted">{t("Step {step} of 3", { step })}</p>
        {step === 1 ? (
          <div>
            <Label htmlFor="new-viewer-name">{t("Their name")}</Label>
            <Input
              id="new-viewer-name"
              value={name}
              onChange={(e) => setName(e.currentTarget.value)}
              autoComplete="off"
              required
            />
          </div>
        ) : null}
        {step === 2 ? (
          <div className="space-y-4">
            <p className="text-sm font-medium text-fg">{t("How will they sign in?")}</p>
            <Segmented
              ariaLabel={t("How they sign in")}
              value={method}
              onChange={(v) => {
                setMethod(v);
                setError("");
              }}
              options={[
                { value: "local", label: t("Username and password"), icon: <KeyRound size={14} /> },
                { value: "invite", label: t("Send an invite link"), icon: <Link2 size={14} /> },
                ...(plexConnected
                  ? [
                      {
                        value: "plex" as Method,
                        label: t("Share with their Plex account"),
                        icon: <MonitorSmartphone size={14} />,
                      },
                    ]
                  : []),
              ]}
            />
            {method === "local" ? (
              <div className="space-y-3">
                <div>
                  <Label htmlFor="new-viewer-username">{t("Username")}</Label>
                  <Input
                    id="new-viewer-username"
                    value={username}
                    onChange={(e) => setUsername(e.currentTarget.value)}
                    autoComplete="off"
                    required
                  />
                </div>
                <div>
                  <Label htmlFor="new-viewer-password">{t("Password")}</Label>
                  <PasswordInput
                    id="new-viewer-password"
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
                  ) : (
                    <p className="mt-1 text-xs text-fg-muted">{t("Use at least 12 characters.")}</p>
                  )}
                </div>
              </div>
            ) : method === "invite" ? (
              <p className="text-sm text-fg-muted">
                {t("They set their own password or link their Plex account at /ether/me.")}
              </p>
            ) : (
              <p className="text-sm text-fg-muted">
                {tx(
                  "Sharing happens on {link}. Share your server with them there and they appear here automatically within a few minutes.",
                  {
                    link: (
                      <a
                        href={PLEX_ACCESS_URL}
                        target="_blank"
                        rel="noopener noreferrer"
                        className="font-medium text-primary hover:underline"
                      >
                        Plex.tv
                      </a>
                    ),
                  },
                )}
              </p>
            )}
          </div>
        ) : null}
        {step === 3 ? (
          <div className="space-y-4">
            <p className="text-sm font-medium text-fg">{t("How to connect")}</p>
            {link ? (
              <div className="space-y-2 rounded-lg border border-border bg-bg-subtle p-3">
                <p className="text-sm font-medium">{t("Send them this link")}</p>
                <p className="break-all text-sm text-fg-muted">{link}</p>
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  onClick={() => {
                    void navigator.clipboard?.writeText(link);
                    toast.success(t("Link copied"));
                  }}
                >
                  <Copy size={14} /> {t("Copy link")}
                </Button>
              </div>
            ) : null}
            {connectCards.map((card) => (
              <section
                key={card.id}
                aria-label={card.title}
                className="space-y-1.5 rounded-lg border border-border bg-bg-subtle p-3"
              >
                <h3 className="text-sm font-medium text-fg">{card.title}</h3>
                {card.lines.map((line) => (
                  <p key={line} className="text-sm text-fg-muted">
                    {line}
                  </p>
                ))}
                {card.id === "plex" && method === "plex" ? (
                  <a
                    href={PLEX_ACCESS_URL}
                    target="_blank"
                    rel="noopener noreferrer"
                    className="inline-block text-sm font-medium text-primary hover:underline"
                  >
                    {t("Open Plex sharing")}
                  </a>
                ) : null}
              </section>
            ))}
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={() => {
                void navigator.clipboard?.writeText(messageText());
                toast.success(t("Message copied"));
              }}
            >
              <MessageSquareText size={14} /> {t("Copy as message")}
            </Button>
          </div>
        ) : null}
        {error ? (
          <p role="alert" className="text-sm text-danger">
            {error}
          </p>
        ) : null}
        <div className="flex justify-end gap-2">
          {step === 2 ? (
            <Button type="button" variant="ghost" onClick={() => setStep(1)}>
              {t("Back")}
            </Button>
          ) : null}
          <Button type="button" variant="ghost" onClick={close}>
            {step === 3 ? t("Done") : t("Cancel")}
          </Button>
          {step < 3 ? (
            <Button
              type="submit"
              disabled={busy || passwordTooShort || (step === 1 && !name.trim())}
            >
              {step === 1 || method === "plex"
                ? t("Continue")
                : method === "local"
                  ? t("Create viewer")
                  : t("Create invite")}
            </Button>
          ) : null}
        </div>
      </form>
    </Drawer>
  );
}

export function Membership({
  id,
  active,
  onChange,
}: {
  id: number;
  active: boolean;
  onChange: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  async function change() {
    setBusy(true);
    setError("");
    try {
      await apiFetch(`/users/${id}/membership`, {
        method: "PATCH",
        body: JSON.stringify({ active: !active }),
      });
      onChange();
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  }
  return (
    <div>
      {error ? (
        <p role="alert" className="text-sm text-danger">
          {error}
        </p>
      ) : null}
      <Button type="button" variant="outline" disabled={busy} onClick={() => void change()}>
        {active ? t("Deactivate viewer") : t("Reactivate viewer")}
      </Button>
    </div>
  );
}
