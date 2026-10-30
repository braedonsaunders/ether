import { Button } from "@braedonsaunders/appkit-ui";
import { useRef, useState } from "react";
import { SettingsRow } from "../../components/SettingsRow";
import { t } from "../../i18n";
import { isPlainObject } from "../../lib/format";

/** Only report addresses actually returned by Plex discovery, not generated candidates. */
export function secureAddresses(remote: unknown) {
  if (!isPlainObject(remote) || !Array.isArray(remote.resourceConnections)) return [];
  const seen = new Set<string>();
  return remote.resourceConnections.flatMap((connection) => {
    if (!isPlainObject(connection) || typeof connection.uri !== "string") return [];
    try {
      const url = new URL(connection.uri);
      if (
        url.protocol !== "https:" ||
        url.username ||
        url.password ||
        url.search ||
        url.hash ||
        (url.pathname !== "" && url.pathname !== "/") ||
        seen.has(url.origin)
      )
        return [];
      seen.add(url.origin);
      return [
        { uri: connection.uri, local: connection.local === true, relay: connection.relay === true },
      ];
    } catch {
      return [];
    }
  });
}

function Address({ uri, label }: { uri: string; label: string }) {
  const input = useRef<HTMLInputElement>(null);
  const [message, setMessage] = useState<"copied" | "select" | null>(null);
  const copy = async () => {
    try {
      if (!navigator.clipboard?.writeText) throw new Error("clipboard unavailable");
      await navigator.clipboard.writeText(uri);
      setMessage("copied");
    } catch {
      // The dashboard can be opened over HTTP, where clipboard access is unavailable.
      input.current?.focus();
      input.current?.select();
      setMessage("select");
    }
  };
  return (
    <li className="space-y-1">
      <label className="block text-xs text-fg-muted">
        {label}
        <input
          ref={input}
          value={uri}
          readOnly
          onFocus={(event) => event.currentTarget.select()}
          className="mt-1 block w-full rounded border border-line bg-transparent p-2 font-mono text-xs text-fg"
        />
      </label>
      <Button variant="outline" size="sm" onClick={() => void copy()}>
        {t("Copy address")}
      </Button>
      <p role="status" className="text-xs text-fg-muted">
        {message === "copied"
          ? t("Address copied")
          : message === "select"
            ? t("Copy the selected address.")
            : null}
      </p>
    </li>
  );
}

export function SecureAddresses({ remote }: { remote: unknown }) {
  const addresses = secureAddresses(remote);
  return (
    <SettingsRow
      stacked
      title={t("HTTPS addresses from Plex")}
      description={t(
        "Use these addresses unchanged. Replacing the hostname with an IP address causes a certificate name mismatch. A listed address does not prove remote access works.",
      )}
    >
      {addresses.length ? (
        <ul className="space-y-3">
          {addresses.map(({ uri, local, relay }) => (
            <Address
              key={uri}
              uri={uri}
              label={relay ? t("Relay") : local ? t("Local network") : t("Remote")}
            />
          ))}
        </ul>
      ) : (
        <p className="text-xs text-fg-muted">
          {t("Plex has not reported any HTTPS addresses yet.")}
        </p>
      )}
    </SettingsRow>
  );
}
