import { t } from "../../i18n";
import { useState, type FormEvent } from "react";
import { Button, Input, Label, toast } from "@braedonsaunders/appkit-ui";
import { Copy, KeyRound, Link2 } from "lucide-react";
import { Drawer } from "../../components/Drawer";
import { PasswordInput, Segmented } from "../../components/fields";
import { apiFetch, errorMessage } from "../../lib/api";
import { MIN_LOCAL_PASSWORD } from "../me/SignInMethods";

/** Add a viewer in a drawer: a local login, or an invite link to share. */
export function AddViewerDrawer({
  open,
  onClose,
  onCreated,
}: {
  open: boolean;
  onClose: () => void;
  onCreated: () => void;
}) {
  const [mode, setMode] = useState<"local" | "invite">("local");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [display, setDisplay] = useState("");
  const [link, setLink] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  function close() {
    setUsername("");
    setPassword("");
    setDisplay("");
    setLink("");
    setError("");
    onClose();
  }
  async function submit(e: FormEvent) {
    e.preventDefault();
    if (mode === "local" && password.length < MIN_LOCAL_PASSWORD) {
      setError(t("Password must be at least 12 characters."));
      return;
    }
    setBusy(true);
    setError("");
    setLink("");
    try {
      if (mode === "local") {
        await apiFetch("/users/local", {
          method: "POST",
          body: JSON.stringify({ username, password }),
        });
        onCreated();
        toast.success(t("Viewer added"));
        close();
      } else {
        const result = await apiFetch<{ url: string }>("/users/invite", {
          method: "POST",
          body: JSON.stringify({ display }),
        });
        setLink(new URL(result.url, window.location.origin).href);
        onCreated();
      }
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  }
  const passwordTooShort =
    mode === "local" && password !== "" && password.length < MIN_LOCAL_PASSWORD;
  return (
    <Drawer open={open} onClose={close} title={t("Add viewer")}>
      <form className="space-y-5" onSubmit={submit}>
        <Segmented
          ariaLabel={t("How they sign in")}
          value={mode}
          onChange={(v) => {
            setMode(v);
            setError("");
            setLink("");
          }}
          options={[
            { value: "local", label: t("Username and password"), icon: <KeyRound size={14} /> },
            { value: "invite", label: t("Invite link"), icon: <Link2 size={14} /> },
          ]}
        />
        {mode === "local" ? (
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
        ) : (
          <div>
            <Label htmlFor="new-viewer-display">{t("Display name")}</Label>
            <Input
              id="new-viewer-display"
              value={display}
              onChange={(e) => setDisplay(e.currentTarget.value)}
              required
            />
          </div>
        )}
        {error ? (
          <p role="alert" className="text-sm text-danger">
            {error}
          </p>
        ) : null}
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
        <div className="flex justify-end gap-2">
          <Button type="button" variant="ghost" onClick={close}>
            {link ? t("Done") : t("Cancel")}
          </Button>
          {link ? null : (
            <Button type="submit" disabled={busy || passwordTooShort}>
              {mode === "local" ? t("Create viewer") : t("Create invite")}
            </Button>
          )}
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
