import { EmptyState } from "@braedonsaunders/appkit-ui";
import { UserRound } from "lucide-react";
import { t } from "../i18n";
import { ErrorPanel, LoadingRows } from "../components/feedback";
import { DomainPage } from "../components/layout";
import { LevelBadge, StatusDot } from "../components/status";
import { api, getToken } from "../lib/api";
import { useAsync } from "../lib/hooks";
import { ConnectApps } from "./me/ConnectApps";
import SignInMethods from "./me/SignInMethods";

/** The signed-in administrator's own sign-in methods, password and devices. */
export default function Profile() {
  const users = useAsync(() => api.users(), []);
  const owner = users.data?.find((user) => user.isOwner);
  const name = owner?.title || owner?.username || "";
  return (
    <DomainPage
      title={t("Your account")}
      base="/profile"
      status={
        owner ? (
          <ul aria-label={t("Status")}>
            <li className="flex items-start gap-3 rounded-xl border border-border bg-surface px-4 py-3">
              <StatusDot level="ok" className="mt-1.5" />
              <span className="min-w-0 flex-1">
                <span className="block truncate text-sm font-medium text-fg">{name}</span>
                <span className="block text-xs text-fg-muted">{t("Owner")}</span>
              </span>
              <LevelBadge level="ok" label={t("Signed in")} />
            </li>
          </ul>
        ) : null
      }
    >
      {users.error ? (
        <ErrorPanel error={users.error} onRetry={() => void users.reload()} />
      ) : !users.data ? (
        <LoadingRows rows={4} />
      ) : !owner ? (
        <EmptyState
          icon={<UserRound />}
          title={t("This server has no owner account yet.")}
          description={t("Finish setup to create the account that administers this server.")}
        />
      ) : (
        <div className="space-y-6">
          <ConnectApps base={`/viewers/${owner.id}`} token={getToken()} />
          <SignInMethods base={`/users/${owner.id}`} token={getToken()} owner showInvite={false} />
        </div>
      )}
    </DomainPage>
  );
}
