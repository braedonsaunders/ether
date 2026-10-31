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
