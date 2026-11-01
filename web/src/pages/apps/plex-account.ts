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
