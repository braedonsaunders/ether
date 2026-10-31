import { t } from "../../i18n";

/** Frontend status fields the owner-console summary actually reads. */
export type ReachabilityStatus = {
  addresses?: string[] | null;
  connectUrls?: string[];
  lan?: string;
  wan?: string;
};

const UNUSABLE_HOSTS = new Set(["", "localhost", "127.0.0.1", "::1", "0.0.0.0", "::"]);

function hostOf(value: string): string {
  const trimmed = value.trim();
  try {
    const url = trimmed.includes("://") ? new URL(trimmed) : new URL(`http://${trimmed}`);
    return url.hostname.replace(/^\[|\]$/g, "").toLowerCase();
  } catch {
    return trimmed.toLowerCase();
  }
}

/** Loopback and unspecified binds are not addresses another device can use. */
export function isLoopbackAddress(value: string): boolean {
  return UNUSABLE_HOSTS.has(hostOf(value));
}

/** Prefer published connect URLs. Fall back to listener addresses that are not loopback. */
export function usableAddresses(status: ReachabilityStatus | null | undefined): string[] {
  const urls = (status?.connectUrls ?? []).filter((item) => item && !isLoopbackAddress(item));
  if (urls.length) return urls;
  return (status?.addresses ?? []).filter((item) => item && !isLoopbackAddress(item));
}

export function homeSummary(status: ReachabilityStatus | null | undefined): {
  title: string;
  detail: string;
} {
  const address = usableAddresses(status).join(", ");
  const lan = status?.lan || "unknown";
  if (lan === "reachable") {
    return {
      title: t("At home"),
      detail: address
        ? t("Reachable at home. Devices on your network use {address}.", { address })
        : t("Reachable at home. Ether has not reported an address other devices can use."),
    };
  }
  if (lan === "unreachable") {
    return {
      title: t("At home"),
      detail: address
        ? t("Not reachable at home. The address to try is {address}.", { address })
        : t("Not reachable at home. No address on your network is listening."),
    };
  }
  if (address) {
    return {
      title: t("At home"),
      detail: t(
        "Not checked yet. Devices on your network should use {address}. Ether has not confirmed they can connect.",
        { address },
      ),
    };
  }
  return {
    title: t("At home"),
    detail: t(
      "Not checked yet. This server is only listening on this computer, so other devices at home cannot reach it.",
    ),
  };
}

export function awaySummary(
  status: ReachabilityStatus | null | undefined,
  options?: { remoteAccess?: boolean; homeNetwork?: boolean },
): { title: string; detail: string } {
  const wan = status?.wan || "unknown";
  if (wan === "reachable") {
    return { title: t("Away from home"), detail: t("Reachable away from home.") };
  }
  if (wan === "unreachable") {
    return { title: t("Away from home"), detail: t("Not reachable away from home.") };
  }
  if (options?.remoteAccess === false) {
    return { title: t("Away from home"), detail: t("Not checked yet: remote access is off.") };
  }
  if (options?.homeNetwork) {
    return {
      title: t("Away from home"),
      detail: t("This app is for devices on your home network. Outside access is not checked."),
    };
  }
  return {
    title: t("Away from home"),
    detail: t(
      "Not checked yet. Ether has not checked whether this server can be reached from outside your home.",
    ),
  };
}

export function claimSummary(claimed: boolean, owner?: string): string {
  if (!claimed) {
    return t("Not claimed. Sign in on the Account tab so Plex apps can find this server.");
  }
  if (owner) return t("Claimed by the Plex account {owner}.", { owner });
  return t("Claimed by your Plex account.");
}
