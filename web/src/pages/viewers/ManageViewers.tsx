import { t } from "../../i18n";
import { useState, type FormEvent } from "react";
import { Button, Input, Label } from "@braedonsaunders/appkit-ui";
import { PasswordInput } from "../../components/fields";
import { apiFetch, errorMessage } from "../../lib/api";

export function CreateViewer({ onCreated }: { onCreated: () => void }) {
  const [mode, setMode] = useState<"local" | "invite">("local");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [display, setDisplay] = useState("");
  const [link, setLink] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError("");
    setLink("");
    try {
      if (mode === "local") {
        await apiFetch("/users/local", {
          method: "POST",
          body: JSON.stringify({ username, password }),
        });
        setUsername("");
        setPassword("");
      } else {
        const result = await apiFetch<{ url: string }>("/users/invite", {
          method: "POST",
          body: JSON.stringify({ display }),
        });
        setDisplay("");
        setLink(new URL(result.url, window.location.origin).href);
      }
      onCreated();
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  }
  return (
    <details className="rounded-lg border border-border bg-surface p-4">
      <summary className="cursor-pointer font-medium">{t("Add viewer")}</summary>
      <form className="mt-4 space-y-3" onSubmit={submit}>
        <label className="block text-sm">
          <input
            type="radio"
            name="viewer-method"
            checked={mode === "local"}
            onChange={() => setMode("local")}
          />{" "}
          {t("Username and password")}
        </label>
        <label className="block text-sm">
          <input
            type="radio"
            name="viewer-method"
            checked={mode === "invite"}
            onChange={() => setMode("invite")}
          />{" "}
          {t("Invite link")}
        </label>
        {mode === "local" ? (
          <>
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
                value={password}
                onChange={(e) => setPassword(e.currentTarget.value)}
                autoComplete="new-password"
                required
              />
            </div>
          </>
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
        {link ? <p className="break-all text-sm">{link}</p> : null}
        <Button type="submit" disabled={busy}>
          {mode === "local" ? t("Create viewer") : t("Create invite")}
        </Button>
      </form>
    </details>
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
