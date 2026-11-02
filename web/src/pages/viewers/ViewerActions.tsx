import {
  Button,
  Input,
  Label,
  confirmDialog,
  toast,
  type ContextMenuEntry,
} from "@braedonsaunders/appkit-ui";
import {
  ChevronDown,
  Copy,
  ExternalLink,
  KeyRound,
  LayoutTemplate,
  Link2,
  LogOut,
  Merge,
  PauseCircle,
  PlayCircle,
  RefreshCw,
  Trash2,
  Tv,
} from "lucide-react";
import { useEffect, useState, type FormEvent } from "react";
import { useNavigate } from "react-router-dom";
import { ActionMenu } from "../../components/ActionMenu";
import { Drawer } from "../../components/Drawer";
import { PasswordInput } from "../../components/fields";
import { viewerName } from "../../components/pickers";
import { t, tn } from "../../i18n";
import { api, apiFetch, errorMessage } from "../../lib/api";
import type { Viewer } from "../../lib/contract";
import { useAsync } from "../../lib/hooks";
import { MIN_LOCAL_PASSWORD } from "../me/SignInMethods";
import { PLEX_ACCESS_URL, viewerLifecycle } from "./ViewerStatus";

type Panel = "invite" | "password" | "tvcode" | "merge" | "remove" | null;

/**
 * The viewer page's one Actions menu: Home, sign-in help, access and removal.
 * What shows follows the viewer's lifecycle — Plex viewers are managed on
 * plex.tv, so Ether offers no pause, remove or merge for them.
 */
export function ViewerActions({
  viewer,
  onChange,
  onRebuild,
  rebuilding,
  onPreviewHome,
}: {
  viewer: Viewer;
  onChange: () => void;
  onRebuild: () => void;
  rebuilding: boolean;
  onPreviewHome: () => void;
}) {
  const [panel, setPanel] = useState<Panel>(null);
  const id = viewer.id;
  const name = viewerName(viewer);
  const life = viewerLifecycle(viewer);
  const plexManaged = life.managedBy === "plex";

  async function signOutEverywhere() {
    const ok = await confirmDialog({
      title: t("Sign out everywhere?"),
      message: t("This signs {name} out of every app and device. They can sign in again.", {
        name,
      }),
      confirmLabel: t("Sign out everywhere"),
    });
    if (!ok) return;
    try {
      const result = await apiFetch<{ revoked: number }>(`/users/${id}/signout`, {
        method: "POST",
        body: JSON.stringify({}),
      });
      toast.success(t("Signed out everywhere"), {
        description: tn(
          result.revoked,
          "{count} sign-in was ended.",
          "{count} sign-ins were ended.",
        ),
      });
    } catch (e) {
      toast.error(t("Couldn't sign them out"), { description: errorMessage(e) });
    }
  }

  async function setPaused(paused: boolean) {
    if (paused) {
      const ok = await confirmDialog({
        title: t("Pause access?"),
        message: t(
          "{name} keeps everything — history, Home and settings — but can't sign in anywhere until you resume access.",
          { name },
        ),
        confirmLabel: t("Pause access"),
      });
      if (!ok) return;
    }
    try {
      await apiFetch(`/users/${id}/${paused ? "pause" : "resume"}`, {
        method: "POST",
        body: JSON.stringify({}),
      });
      toast.success(paused ? t("Access paused") : t("Access resumed"));
      onChange();
    } catch (e) {
      toast.error(paused ? t("Couldn't pause access") : t("Couldn't resume access"), {
        description: errorMessage(e),
      });
    }
  }

  const items: ContextMenuEntry[] = [
    {
      key: "rebuild",
      label: t("Rebuild rows"),
      icon: RefreshCw,
      disabled: rebuilding,
      onSelect: onRebuild,
    },
    {
      key: "preview-home",
      label: t("Preview their Home"),
      icon: LayoutTemplate,
      onSelect: onPreviewHome,
    },
    { key: "sep-signin", separator: true },
    ...(!plexManaged && !viewer.isOwner
      ? [
          {
            key: "invite",
            label: t("Send invite link"),
            icon: Link2,
            onSelect: () => setPanel("invite"),
          } satisfies ContextMenuEntry,
        ]
      : []),
    ...(!plexManaged
      ? [
          {
            key: "password",
            label: t("Reset password"),
            icon: KeyRound,
            onSelect: () => setPanel("password"),
          } satisfies ContextMenuEntry,
        ]
      : []),
    {
      key: "tvcode",
      label: t("Approve a TV code"),
      icon: Tv,
      onSelect: () => setPanel("tvcode"),
    },
    {
      key: "signout",
      label: t("Sign out everywhere"),
      icon: LogOut,
      onSelect: () => void signOutEverywhere(),
    },
    ...(plexManaged
      ? [
          { key: "sep-plex", separator: true } satisfies ContextMenuEntry,
          {
            key: "plex",
            label: t("Manage on plex.tv"),
            icon: ExternalLink,
            onSelect: () => window.open(PLEX_ACCESS_URL, "_blank", "noopener,noreferrer"),
          } satisfies ContextMenuEntry,
        ]
      : [
          ...(life.canPause
            ? [
                { key: "sep-access", separator: true } satisfies ContextMenuEntry,
                life.paused
                  ? {
                      key: "resume",
                      label: t("Resume access"),
                      icon: PlayCircle,
                      onSelect: () => void setPaused(false),
                    }
                  : {
                      key: "pause",
                      label: t("Pause access"),
                      icon: PauseCircle,
                      onSelect: () => void setPaused(true),
                    },
              ]
            : []),
          ...(life.canMerge || life.canRemove
            ? [{ key: "sep-remove", separator: true } satisfies ContextMenuEntry]
            : []),
          ...(life.canMerge
            ? [
                {
                  key: "merge",
                  label: t("Merge into another viewer"),
                  icon: Merge,
                  onSelect: () => setPanel("merge"),
                } satisfies ContextMenuEntry,
              ]
            : []),
          ...(life.canRemove
            ? [
                {
                  key: "remove",
                  label: t("Remove viewer"),
                  icon: Trash2,
                  danger: true,
                  onSelect: () => setPanel("remove"),
                } satisfies ContextMenuEntry,
              ]
            : []),
        ]),
  ];

  return (
    <>
      <ActionMenu items={items} variant="outline" size="sm" label={t("Actions")}>
        {t("Actions")} <ChevronDown size={14} />
      </ActionMenu>
      <InvitePanel viewerId={id} open={panel === "invite"} onClose={() => setPanel(null)} />
      <ResetPasswordPanel
        viewer={viewer}
        open={panel === "password"}
        onClose={() => setPanel(null)}
      />
      <TvCodePanel viewer={viewer} open={panel === "tvcode"} onClose={() => setPanel(null)} />
      <MergePanel viewer={viewer} open={panel === "merge"} onClose={() => setPanel(null)} />
      <RemovePanel viewer={viewer} open={panel === "remove"} onClose={() => setPanel(null)} />
    </>
  );
}

function InvitePanel({
  viewerId,
  open,
  onClose,
}: {
  viewerId: number;
  open: boolean;
  onClose: () => void;
}) {
  const [link, setLink] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    if (!open) return;
    let cancelled = false;
    setLink("");
    setError("");
    setBusy(true);
    apiFetch<{ url: string }>(`/users/${viewerId}/invite`, {
      method: "POST",
      body: JSON.stringify({}),
    })
      .then((result) => {
        if (!cancelled) setLink(new URL(result.url, window.location.origin).href);
      })
      .catch((e: unknown) => {
        if (!cancelled) setError(errorMessage(e));
      })
      .finally(() => {
        if (!cancelled) setBusy(false);
      });
    return () => {
      cancelled = true;
    };
  }, [open, viewerId]);
  return (
    <Drawer open={open} onClose={onClose} title={t("Send invite link")}>
      <div className="space-y-4">
        <p className="text-sm text-fg-muted">
          {t("Send them this link so they can finish setting up their sign-in.")}
        </p>
        {error ? (
          <p role="alert" className="text-sm text-danger">
            {error}
          </p>
        ) : busy || !link ? (
          <p className="text-sm text-fg-muted">{t("Creating the link…")}</p>
        ) : (
          <div className="space-y-2 rounded-lg border border-border bg-bg-subtle p-3">
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
        )}
        <div className="flex justify-end">
          <Button type="button" variant="ghost" onClick={onClose}>
            {t("Done")}
          </Button>
        </div>
      </div>
    </Drawer>
  );
}

function ResetPasswordPanel({
  viewer,
  open,
  onClose,
}: {
  viewer: Viewer;
  open: boolean;
  onClose: () => void;
}) {
  const [username, setUsername] = useState(viewer.username);
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    if (open) {
      setUsername(viewer.username);
      setPassword("");
      setError("");
    }
  }, [open, viewer.username]);
  const tooShort = password !== "" && password.length < MIN_LOCAL_PASSWORD;
  async function submit(e: FormEvent) {
    e.preventDefault();
    if (password.length < MIN_LOCAL_PASSWORD) {
      setError(t("Password must be at least 12 characters."));
      return;
    }
    setBusy(true);
    setError("");
    try {
      await apiFetch(`/users/${viewer.id}/password`, {
        method: "POST",
        body: JSON.stringify({ username, password }),
      });
      toast.success(t("Password saved."));
      onClose();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  }
  return (
    <Drawer open={open} onClose={onClose} title={t("Reset password")}>
      <form className="space-y-4" onSubmit={(e) => void submit(e)}>
        <div>
          <Label htmlFor="reset-username">{t("Username")}</Label>
          <Input
            id="reset-username"
            value={username}
            onChange={(e) => setUsername(e.currentTarget.value)}
            autoComplete="username"
            required
          />
        </div>
        <div>
          <Label htmlFor="reset-password">{t("New password")}</Label>
          <PasswordInput
            id="reset-password"
            aria-label={t("New password")}
            aria-invalid={tooShort || undefined}
            minLength={MIN_LOCAL_PASSWORD}
            value={password}
            onChange={(e) => setPassword(e.currentTarget.value)}
            autoComplete="new-password"
            required
          />
          {tooShort ? (
            <p className="mt-1 text-xs text-danger" role="alert">
              {t("Password must be at least 12 characters.")}
            </p>
          ) : (
            <p className="mt-1 text-xs text-fg-muted">{t("Use at least 12 characters.")}</p>
          )}
        </div>
        {error ? (
          <p role="alert" className="text-sm text-danger">
            {error}
          </p>
        ) : null}
        <div className="flex justify-end gap-2">
          <Button type="button" variant="ghost" onClick={onClose}>
            {t("Cancel")}
          </Button>
          <Button type="submit" disabled={busy || tooShort}>
            {t("Set or reset password")}
          </Button>
        </div>
      </form>
    </Drawer>
  );
}

function TvCodePanel({
  viewer,
  open,
  onClose,
}: {
  viewer: Viewer;
  open: boolean;
  onClose: () => void;
}) {
  const [code, setCode] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    if (open) {
      setCode("");
      setError("");
    }
  }, [open]);
  async function submit(e: FormEvent) {
    e.preventDefault();
    const value = code.trim().toUpperCase();
    if (value.length !== 6) {
      setError(t("Enter the 6-character code shown on the TV."));
      return;
    }
    setBusy(true);
    setError("");
    try {
      await apiFetch(`/viewers/${viewer.id}/quickconnect`, {
        method: "POST",
        body: JSON.stringify({ code: value }),
      });
      toast.success(t("TV approved"), {
        description: t("The TV is signed in as {name}.", { name: viewerName(viewer) }),
      });
      onClose();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  }
  return (
    <Drawer open={open} onClose={onClose} title={t("Approve a TV code")}>
      <form className="space-y-4" onSubmit={(e) => void submit(e)}>
        <p className="text-sm text-fg-muted">
          {t("Enter the 6-character code shown on the TV to sign it in as {name}.", {
            name: viewerName(viewer),
          })}
        </p>
        <div>
          <Label htmlFor="tv-code">{t("TV code")}</Label>
          <Input
            id="tv-code"
            value={code}
            onChange={(e) => setCode(e.currentTarget.value.toUpperCase())}
            maxLength={6}
            autoComplete="off"
            required
          />
        </div>
        {error ? (
          <p role="alert" className="text-sm text-danger">
            {error}
          </p>
        ) : null}
        <div className="flex justify-end gap-2">
          <Button type="button" variant="ghost" onClick={onClose}>
            {t("Cancel")}
          </Button>
          <Button type="submit" disabled={busy || code.trim().length !== 6}>
            {t("Approve")}
          </Button>
        </div>
      </form>
    </Drawer>
  );
}

function MergePanel({
  viewer,
  open,
  onClose,
}: {
  viewer: Viewer;
  open: boolean;
  onClose: () => void;
}) {
  const navigate = useNavigate();
  const viewers = useAsync(() => (open ? api.viewers() : Promise.resolve([])), [open]);
  const [into, setInto] = useState<number | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const name = viewerName(viewer);
  const choices = (viewers.data ?? []).filter((v) => v.id !== viewer.id && !v.isOwner);
  const dest = choices.find((v) => v.id === into);
  async function submit() {
    if (!dest) return;
    const ok = await confirmDialog({
      title: t("Merge {name} into {dest}?", { name, dest: viewerName(dest) }),
      message: t("Everything moves to {dest} and {name} is removed. This can't be undone.", {
        name,
        dest: viewerName(dest),
      }),
      confirmLabel: t("Merge"),
      tone: "danger",
    });
    if (!ok) return;
    setBusy(true);
    setError("");
    try {
      await apiFetch(`/users/${viewer.id}/merge`, {
        method: "POST",
        body: JSON.stringify({ into: dest.id }),
      });
      toast.success(t("Viewers merged"));
      onClose();
      navigate(`/viewers/${dest.id}/overview`);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  }
  return (
    <Drawer open={open} onClose={onClose} title={t("Merge into another viewer")}>
      <div className="space-y-4">
        <p className="text-sm text-fg-muted">
          {t(
            "Use this when one person ended up with two viewers. Watch history, resume points, ratings, watchlist, favourites, taste, sign-in methods and devices move to the viewer you pick. Where both have something, the viewer you pick wins, except plays, which are combined. Then {name} is removed.",
            { name },
          )}
        </p>
        <div role="radiogroup" aria-label={t("Merge into")}>
          {viewers.error ? (
            <p role="alert" className="text-sm text-danger">
              {errorMessage(viewers.error)}
            </p>
          ) : !viewers.data ? (
            <p className="text-sm text-fg-muted">{t("Loading viewers")}</p>
          ) : choices.length === 0 ? (
            <p className="text-sm text-fg-muted">{t("There is no other viewer to merge into.")}</p>
          ) : (
            <ul className="divide-y divide-border rounded-xl border border-border">
              {choices.map((v) => (
                <li key={v.id}>
                  <button
                    type="button"
                    role="radio"
                    aria-checked={into === v.id}
                    className="flex w-full items-center justify-between px-3 py-2 text-left text-sm hover:bg-bg-subtle"
                    onClick={() => setInto(v.id)}
                  >
                    <span className="font-medium text-fg">{viewerName(v)}</span>
                    <span className="text-xs text-fg-muted">{v.username}</span>
                  </button>
                </li>
              ))}
            </ul>
          )}
        </div>
        {error ? (
          <p role="alert" className="text-sm text-danger">
            {error}
          </p>
        ) : null}
        <div className="flex justify-end gap-2">
          <Button type="button" variant="ghost" onClick={onClose}>
            {t("Cancel")}
          </Button>
          <Button type="button" disabled={busy || !dest} onClick={() => void submit()}>
            {t("Merge")}
          </Button>
        </div>
      </div>
    </Drawer>
  );
}

function RemovePanel({
  viewer,
  open,
  onClose,
}: {
  viewer: Viewer;
  open: boolean;
  onClose: () => void;
}) {
  const navigate = useNavigate();
  const [typed, setTyped] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const name = viewerName(viewer);
  useEffect(() => {
    if (open) {
      setTyped("");
      setError("");
    }
  }, [open]);
  const matches = typed.trim() === name;
  async function submit(e: FormEvent) {
    e.preventDefault();
    if (!matches) return;
    setBusy(true);
    setError("");
    try {
      await apiFetch(`/users/${viewer.id}`, { method: "DELETE" });
      toast.success(t("Viewer removed"));
      onClose();
      navigate("/viewers");
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  }
  return (
    <Drawer open={open} onClose={onClose} title={t("Remove viewer")}>
      <form className="space-y-4" onSubmit={(e) => void submit(e)}>
        <p className="text-sm text-fg-muted">
          {t(
            "This deletes {name} and all of their data: watch history, resume points, ratings, watchlist, favourites, taste, sign-in methods and devices. This can't be undone. To keep everything but stop access, pause them instead.",
            { name },
          )}
        </p>
        <div>
          <Label htmlFor="remove-name">{t("Type their name to confirm")}</Label>
          <Input
            id="remove-name"
            value={typed}
            onChange={(e) => setTyped(e.currentTarget.value)}
            placeholder={name}
            autoComplete="off"
          />
        </div>
        {error ? (
          <p role="alert" className="text-sm text-danger">
            {error}
          </p>
        ) : null}
        <div className="flex justify-end gap-2">
          <Button type="button" variant="ghost" onClick={onClose}>
            {t("Cancel")}
          </Button>
          <Button type="submit" variant="destructive" disabled={busy || !matches}>
            {t("Remove viewer")}
          </Button>
        </div>
      </form>
    </Drawer>
  );
}
