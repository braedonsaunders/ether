import { t } from "../i18n";
import { ErrorPanel, LoadingRows } from "../components/feedback";
import { DomainPage } from "../components/layout";
import { api, getToken } from "../lib/api";
import { useAsync } from "../lib/hooks";
import SignInMethods from "./me/SignInMethods";

/** The signed-in administrator's own sign-in methods, password and devices. */
export default function Profile() {
  const users = useAsync(() => api.users(), []);
  const owner = users.data?.find((user) => user.isOwner);
  return (
    <DomainPage
      title={t("Your account")}
      description={t(
        "How you sign in to Ether, and the phones, TVs and browsers that stay signed in.",
      )}
      base="/profile"
    >
      {users.error ? (
        <ErrorPanel error={users.error} onRetry={() => void users.reload()} />
      ) : !users.data ? (
        <LoadingRows rows={4} />
      ) : !owner ? (
        <p className="text-sm text-fg-muted">{t("This server has no owner account yet.")}</p>
      ) : (
        <SignInMethods base={`/users/${owner.id}`} token={getToken()} owner showInvite={false} />
      )}
    </DomainPage>
  );
}
