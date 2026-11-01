import { useEffect, useState } from "react";
import { Button } from "@braedonsaunders/appkit-ui";
import { t } from "../../i18n";

type Connection = {
  surface: string;
  url: string;
  username?: string;
  password?: string;
  qr: string;
};
const names: Record<string, string> = {
  jellyfin: "Jellyfin",
  emby: "Emby",
  silo: "Silo",
  stremio: "Stremio",
  webdav: "WebDAV",
  m3u: "M3U",
};

export function ConnectApps(props: { base: string; token: string }) {
  // A secret belongs to one viewer, even if a mint finishes after navigation.
  return <ConnectAppsSession key={`${props.base}\u0000${props.token}`} {...props} />;
}

function ConnectAppsSession({ base, token }: { base: string; token: string }) {
  const [apps, setApps] = useState<string[]>([]);
  const [connection, setConnection] = useState<Connection>();
  const [showQR, setShowQR] = useState(false);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    let active = true;
    void fetch(`/ether/api${base}/apps`, { headers: { Authorization: `Bearer ${token}` } })
      .then((response) =>
        response.ok ? (response.json() as Promise<{ apps: string[] }>) : Promise.reject(),
      )
      .then((result) => {
        if (active) setApps(Array.isArray(result.apps) ? result.apps : []);
      })
      .catch(() => {
        if (active) setError(t("The request could not be completed. Try again."));
      });
    return () => {
      active = false;
    };
  }, [base, token]);
  async function create(surface: string) {
    setConnection(undefined);
    setShowQR(false);
    setError("");
    setBusy(true);
    try {
      const response = await fetch(`/ether/api${base}/apps/${surface}`, {
        method: "POST",
        headers: { Authorization: `Bearer ${token}`, "X-Ether-Request": "1" },
      });
      if (!response.ok) {
        const body = (await response.json().catch(() => ({}))) as { error?: { code?: string } };
        if (body.error?.code === "stremio_https_required") {
          setError(t("Stremio needs HTTPS. Add a certificate under Apps → Secure connections."));
          return;
        }
        throw new Error();
      }
      setConnection((await response.json()) as Connection);
    } catch {
      setError(t("The request could not be completed. Try again."));
    } finally {
      setBusy(false);
    }
  }
  if (!apps.length && !error) return null;
  return (
    <section className="space-y-3">
      <div className="space-y-1">
        <h2 className="text-lg font-semibold">{t("Connect apps")}</h2>
        <p className="text-sm text-fg-muted">
          {t("Server address and app password for Infuse, Swiftfin, and other apps.")}
        </p>
      </div>
      {apps
        .filter((id) => names[id])
        .map((id) => (
          <div key={id} className="rounded border border-border p-3 space-y-2">
            <p>{names[id]}</p>
            <Button disabled={busy} onClick={() => void create(id)}>
              {t("Create connection")}
            </Button>
            {connection?.surface === id && (
              <div className="space-y-2">
                <p>{t("This secret is shown only once.")}</p>
                <code>{connection.url}</code>
                {connection.username && (
                  <p>
                    {t("Username")}: {connection.username}
                  </p>
                )}
                {connection.password && (
                  <p>
                    {t("Password")}: <code>{connection.password}</code>
                  </p>
                )}
                <Button
                  onClick={() =>
                    void navigator.clipboard.writeText(connection.password || connection.url)
                  }
                >
                  {t("Copy token")}
                </Button>
                <Button onClick={() => setShowQR(!showQR)}>{t("Show QR code")}</Button>
                {showQR && (
                  <img
                    src={connection.qr}
                    alt={t("QR code for the server address")}
                    width={192}
                    height={192}
                  />
                )}
                {id === "stremio" && (
                  <a href={connection.url.replace(/^https?:\/\//, "stremio://")}>
                    {t("Open in Stremio")}
                  </a>
                )}
              </div>
            )}
          </div>
        ))}
      {error && <p role="alert">{error}</p>}
    </section>
  );
}
