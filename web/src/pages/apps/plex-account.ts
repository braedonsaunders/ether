import { t } from "../../i18n";
import { ApiError } from "../../lib/api";

/** Whether this server is linked to a Plex account.
 *
 * The link is real only when Ether holds a server token. A claimed flag with
 * no token is not claimed. Signing out clears the flag before the token, so a
 * token left behind a cleared flag is not claimed either.
 */
export function plexAccountClaimed(
  plex: { claimed?: boolean; token?: string } | null | undefined,
): boolean {
  const token = plex?.token;
  return plex?.claimed !== false && typeof token === "string" && token !== "";
}

/** Translated owner-facing message when claiming the server fails. */
export function claimFailureMessage(error: unknown): string {
  if (error instanceof ApiError && error.code === "plex_claim_failed") {
    return t("Plex couldn't link this server. Try again in a minute.");
  }
  const detail = error instanceof Error ? error.message : String(error);
  if (detail === "Plex couldn't link this server. Try again in a minute.") {
    return t("Plex couldn't link this server. Try again in a minute.");
  }
  return t("Signed in, but claiming failed: {error}", { error: detail });
}

function isValidCustomServerUrl(raw: string): boolean {
  const s = raw.trim();
  if (!s || s.length > 512 || s.startsWith("[") || /[\s()<>\"'\\|?#]/.test(s)) {
    return false;
  }
  let u: URL;
  try {
    u = new URL(s);
  } catch {
    return false;
  }
  if (u.protocol !== "http:" && u.protocol !== "https:") return false;
  if (u.username || u.password || u.search || u.hash) return false;
  if (u.pathname !== "" && u.pathname !== "/") return false;
  if (u.host.endsWith(":")) return false;
  const host = u.hostname;
  if (!host) return false;
  if (host.startsWith("[") && host.endsWith("]")) {
    return host.length > 2 && host.includes(":");
  }
  if (/[[\]]/.test(u.host)) return false;
  if (/^\d+\.\d+\.\d+\.\d+$/.test(host)) {
    return host.split(".").every((part) => {
      const n = Number(part);
      return Number.isInteger(n) && n >= 0 && n <= 255 && String(n) === part;
    });
  }
  if (host.length > 253 || host.startsWith(".") || host.endsWith(".")) return false;
  const labels = host.split(".");
  for (const label of labels) {
    if (
      !label ||
      label.length > 63 ||
      label.startsWith("-") ||
      label.endsWith("-") ||
      !/^[a-zA-Z0-9-]+$/.test(label)
    ) {
      return false;
    }
  }
  const last = labels[labels.length - 1]!;
  if (/^\d+$/.test(last)) return false;
  if (u.port) {
    const port = Number(u.port);
    if (!Number.isInteger(port) || port < 1 || port > 65535) return false;
  }
  return true;
}

/** Validates custom server URLs at input and returns a translated message when invalid. */
export function validateCustomUrls(value: unknown): string | undefined {
  if (!Array.isArray(value)) return undefined;
  for (const entry of value) {
    if (typeof entry !== "string") continue;
    const trimmed = entry.trim();
    if (!trimmed) continue;
    if (!isValidCustomServerUrl(trimmed)) {
      return t("Enter full http:// or https:// server URLs, separated by commas.");
    }
  }
  return undefined;
}

/** Translates known server field validation messages on the Plex settings page. */
export function translatePlexFieldError(message: string): string {
  if (message === "Enter full http:// or https:// server URLs, separated by commas.") {
    return t("Enter full http:// or https:// server URLs, separated by commas.");
  }
  return message;
}
