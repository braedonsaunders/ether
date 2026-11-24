import { copyText } from "../../components/CopyButton";
import {
  Button,
  Input,
  Label,
  SearchSelect,
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
import { useEffect, useId, useState, type FormEvent } from "react";
import { useNavigate } from "react-router-dom";
import { ActionMenu } from "../../components/ActionMenu";
import { Drawer } from "../../components/Drawer";
import { PasswordInput } from "../../components/fields";
import { viewerName } from "../../components/pickers";
import { searchSelectLabels } from "../../i18n/appkit-labels";
import { t, tn } from "../../i18n";
import { api, ApiError, apiFetch, errorMessage } from "../../lib/api";
import type { Viewer } from "../../lib/contract";
import { useAsync } from "../../lib/hooks";
import { MIN_LOCAL_PASSWORD } from "../me/SignInMethods";
import { PLEX_ACCESS_URL, viewerLifecycle } from "./ViewerStatus";
import { PlexMovePanel } from "../apps/plex-moves";

type Panel = "invite" | "password" | "tvcode" | "copy" | "merge" | "remove" | "plexmove" | null;

/**
 * The viewer page's one Actions menu: Home, sign-in help, access and removal.
 * What shows follows the viewer's lifecycle — Plex viewers can be paused and
 * resumed in Ether, while removal and merging remain managed on plex.tv.
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
        message: plexManaged
          ? t(
              "{name} keeps everything — history, Home and settings — and stays shared on plex.tv, but is signed out everywhere and can't watch until you resume access.",
              { name },
            )
          : t(
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
      if (paused) {
        toast.success(t("Access paused"));
      } else if (plexManaged && life.plexShared === false) {
        toast.success(t("Owner pause removed"), {
          description: t(
            "{name} is still not shared on plex.tv, so they won't have access until you share the server with them there.",
            { name },
          ),
        });
      } else {
        toast.success(t("Access resumed"));
      }
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
    ...(!viewer.isOwner
      ? [
          {
            key: "invite",
            // A Plex viewer gets a local sign-in too, so Jellyfin, Emby and
            // Silo keep working if Plex stops: same viewer, same history.
            label: plexManaged
              ? t("Add a local sign-in (Jellyfin, Emby, Silo)")
              : t("Send invite link"),
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
    ...(!viewer.isOwner
      ? [
          {
            key: "copy",
            label: t("Copy settings from another viewer"),
            icon: Copy,
            onSelect: () => setPanel("copy"),
          } satisfies ContextMenuEntry,
        ]
      : []),
    {
      key: "signout",
      label: t("Sign out everywhere"),
      icon: LogOut,
      onSelect: () => void signOutEverywhere(),
    },
    ...(life.canPause
      ? [
          { key: "sep-access", separator: true } satisfies ContextMenuEntry,
          life.paused
            ? ({
                key: "resume",
                label: t("Resume access"),
                icon: PlayCircle,
                onSelect: () => void setPaused(false),
              } satisfies ContextMenuEntry)
            : ({
                key: "pause",
                label: t("Pause access"),
                icon: PauseCircle,
                onSelect: () => void setPaused(true),
              } satisfies ContextMenuEntry),
        ]
      : []),
    ...(plexManaged
      ? [
          { key: "sep-plex", separator: true } satisfies ContextMenuEntry,
          {
            key: "plexmove",
            label: t("Move to another Plex server"),
            icon: ExternalLink,
            onSelect: () => setPanel("plexmove"),
          } satisfies ContextMenuEntry,
          {
            key: "plex",
            label: t("Manage on plex.tv"),
            icon: ExternalLink,
            onSelect: () => window.open(PLEX_ACCESS_URL, "_blank", "noopener,noreferrer"),
          } satisfies ContextMenuEntry,
        ]
      : [
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
      <PlexMovePanel
        viewerId={id}
        open={panel === "plexmove"}
        onClose={() => setPanel(null)}
        onDone={onChange}
      />
      <ResetPasswordPanel
        viewer={viewer}
        open={panel === "password"}
        onClose={() => setPanel(null)}
      />
      <TvCodePanel viewer={viewer} open={panel === "tvcode"} onClose={() => setPanel(null)} />
      <CopySettingsPanel
        viewer={viewer}
        open={panel === "copy"}
        onClose={() => setPanel(null)}
        onDone={onChange}
      />
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
                void copyText(link);
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
  const [confirm, setConfirm] = useState("");
  const identities = useAsync(
    () =>
      open
        ? apiFetch<{ identities: { provider: string; display: string }[] }>(
            `/users/${viewer.id}/identities`,
          )
        : Promise.resolve({ identities: [] }),
    [open, viewer.id],
  );
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    if (open) {
      setUsername(viewer.username);
      setConfirm("");
      setPassword("");
      setError("");
    }
  }, [open, viewer.username]);
  const tooShort = password !== "" && password.length < MIN_LOCAL_PASSWORD;
  async function submit(e: FormEvent) {
    e.preventDefault();
    if (password !== confirm) {
      setError(t("Passwords do not match."));
      return;
    }
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
          <SearchSelect
            ariaLabel={t("Username")}
            value={username}
            onChange={setUsername}
            searchable
            options={[
              ...new Set([
                viewer.username,
                ...(identities.data?.identities ?? [])
                  .filter((i) => i.provider === "local")
                  .map((i) => i.display),
              ]),
            ]
              .filter(Boolean)
              .map((value) => ({ value, label: value }))}
            {...searchSelectLabels()}
          />
        </div>
        <div>
          <Label htmlFor="reset-password">{t("New password")}</Label>
          <PasswordInput
            id="reset-password"
            aria-label={t("New password")}
            aria-invalid={tooShort || undefined}
            aria-describedby="reset-password-hint"
            minLength={MIN_LOCAL_PASSWORD}
            value={password}
            onChange={(e) => setPassword(e.currentTarget.value)}
            autoComplete="new-password"
            required
          />
          {tooShort ? (
            <p id="reset-password-hint" className="mt-1 text-xs text-danger" role="alert">
              {t("Password must be at least 12 characters.")}
            </p>
          ) : (
            <p id="reset-password-hint" className="mt-1 text-xs text-fg-muted">
              {t("Use at least 12 characters.")}
            </p>
          )}
        </div>
        <div>
          <Label htmlFor="reset-confirm">{t("Confirm new password")}</Label>
          <PasswordInput
            id="reset-confirm"
            aria-invalid={(confirm !== "" && confirm !== password) || undefined}
            aria-describedby={
              confirm && confirm !== password ? "reset-password-mismatch" : undefined
            }
            aria-label={t("Confirm new password")}
            value={confirm}
            onChange={(e) => setConfirm(e.currentTarget.value)}
            autoComplete="new-password"
            required
          />
          {confirm && confirm !== password ? (
            <p id="reset-password-mismatch" role="alert" className="mt-1 text-xs text-danger">
              {t("Passwords do not match.")}
            </p>
          ) : null}
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
          <Button type="submit" disabled={busy || tooShort || !password || password !== confirm}>
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
  return (
    <Drawer open={open} onClose={onClose} title={t("Approve a TV code")}>
      {open && <TvCodeForm viewer={viewer} onApproved={onClose} onCancel={onClose} />}
    </Drawer>
  );
}

export function TvCodeForm({
  viewer,
  onApproved,
  onCancel,
}: {
  viewer: Viewer;
  onApproved?: () => void;
  onCancel?: () => void;
}) {
  const inputId = useId();
  const [success, setSuccess] = useState(false);
  const [code, setCode] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    setCode("");
    setError("");
    setSuccess(false);
  }, [viewer.id]);
  async function submit(e: FormEvent) {
    e.preventDefault();
    const value = code.trim().toUpperCase();
    if (value.length !== 6) {
      setError(t("Enter the 6-character code shown on the TV."));
      return;
    }
    setBusy(true);
    setError("");
    setSuccess(false);
    try {
      await apiFetch(`/viewers/${viewer.id}/quickconnect`, {
        method: "POST",
        body: JSON.stringify({ code: value }),
      });
      toast.success(t("TV approved"), {
        description: t("The TV is signed in as {name}.", { name: viewerName(viewer) }),
      });
      setCode("");
      setSuccess(true);
      onApproved?.();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  }
  return (
    <form className="space-y-4" onSubmit={(e) => void submit(e)}>
      <p className="text-sm text-fg-muted">
        {t("Enter the 6-character code shown on the TV to sign it in as {name}.", {
          name: viewerName(viewer),
        })}
      </p>
      <div>
        <Label htmlFor={inputId}>{t("TV code")}</Label>
        <Input
          id={inputId}
          value={code}
          onChange={(e) => setCode(e.currentTarget.value.toUpperCase())}
          maxLength={6}
          autoComplete="off"
          required
        />
      </div>
      {success ? (
        <p role="status" className="text-sm text-success">
          {t("TV approved")}
        </p>
      ) : null}
      {error ? (
        <p role="alert" className="text-sm text-danger">
          {error}
        </p>
      ) : null}
      <div className="flex justify-end gap-2">
        {onCancel && (
          <Button type="button" variant="ghost" onClick={onCancel}>
            {t("Cancel")}
          </Button>
        )}
        <Button type="submit" disabled={busy || code.trim().length !== 6}>
          {t("Approve")}
        </Button>
      </div>
    </form>
  );
}

/**
 * Uses another viewer as a template: their limits, libraries, parental
 * controls, audio and subtitle choices and Live TV access are written onto
 * this viewer. Identity, sign-in methods, history and taste are not touched.
 */
function CopySettingsPanel({
  viewer,
  open,
  onClose,
  onDone,
}: {
  viewer: Viewer;
  open: boolean;
  onClose: () => void;
  onDone: () => void;
}) {
  const viewers = useAsync(() => (open ? api.viewers() : Promise.resolve([])), [open]);
  const [from, setFrom] = useState<number | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    if (open) {
      setFrom(null);
      setError("");
    }
  }, [open, viewer.id]);
  const name = viewerName(viewer);
  const choices = (viewers.data ?? []).filter((v) => v.id !== viewer.id);
  const source = choices.find((v) => v.id === from);
  async function submit() {
    if (!source) return;
    const ok = await confirmDialog({
      title: t("Copy {source}'s settings to {name}?", { source: viewerName(source), name }),
      message: t(
        "{name}'s stream limit, rating limit, libraries, parental controls, audio and subtitle choices and Live TV access are replaced with {source}'s.",
        { name, source: viewerName(source) },
      ),
      confirmLabel: t("Copy settings"),
    });
    if (!ok) return;
    setBusy(true);
    setError("");
    try {
      const [users, safetyPolicy, streamPrefs, liveTVPolicy] = await Promise.all([
        api.users(),
        apiFetch<unknown>(`/safety/policies/${source.id}`),
        apiFetch<unknown>(`/stream-preferences/${source.id}`),
        apiFetch<unknown>(`/livetv/policies/${source.id}`).catch((err: unknown) => {
          if (err instanceof ApiError && err.status === 404) return null;
          throw err;
        }),
      ]);
      const template = users.find((u) => u.id === source.id);
      if (!template) throw new Error(t("That viewer no longer exists."));
      await api.updateUser(viewer.id, {
        streamLimit: template.streamLimit,
        maxContentRating: template.maxContentRating,
        allowedLibraries: template.allowedLibraries ?? [],
      });
      await apiFetch(`/safety/policies/${viewer.id}`, {
        method: "PUT",
        body: JSON.stringify(safetyPolicy),
      });
      await apiFetch(`/stream-preferences/${viewer.id}`, {
        method: "PUT",
        body: JSON.stringify(streamPrefs),
      });
      if (liveTVPolicy !== null) {
        await apiFetch(`/livetv/policies/${viewer.id}`, {
          method: "PUT",
          body: JSON.stringify(liveTVPolicy),
        });
      }
      toast.success(t("Settings copied"), {
        description: t("{name} now has {source}'s settings.", {
          name,
          source: viewerName(source),
        }),
      });
      onDone();
      onClose();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  }
  return (
    <Drawer open={open} onClose={onClose} title={t("Copy settings from another viewer")}>
      <div className="space-y-4">
        <p className="text-sm text-fg-muted">
          {t(
            "Pick a viewer to use as the template. Their stream limit, rating limit, allowed libraries, parental controls, audio and subtitle choices and Live TV access are copied to {name}. Sign-in, watch history and taste stay as they are.",
            { name },
          )}
        </p>
        <div role="radiogroup" aria-label={t("Copy from")}>
          {viewers.error ? (
            <p role="alert" className="text-sm text-danger">
              {errorMessage(viewers.error)}
            </p>
          ) : !viewers.data ? (
            <p className="text-sm text-fg-muted">{t("Loading viewers")}</p>
          ) : choices.length === 0 ? (
            <p className="text-sm text-fg-muted">{t("There is no other viewer to copy from.")}</p>
          ) : (
            <ul className="divide-y divide-border rounded-xl border border-border">
              {choices.map((v) => (
                <li key={v.id}>
                  <button
                    type="button"
                    role="radio"
                    aria-checked={from === v.id}
                    className={`flex w-full items-center justify-between px-3 py-2 text-left text-sm hover:bg-bg-subtle ${
                      from === v.id ? "bg-bg-subtle" : ""
                    }`}
                    onClick={() => setFrom(v.id)}
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
          <Button type="button" disabled={busy || !source} onClick={() => void submit()}>
            {t("Copy settings")}
          </Button>
        </div>
      </div>
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
