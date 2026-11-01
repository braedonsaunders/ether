import {
  Alert,
  AlertDescription,
  AlertTitle,
  Button,
  Checkbox,
  Input,
} from "@braedonsaunders/appkit-ui";
import { Check, Database, Network, Server } from "lucide-react";
import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { useAppOptional } from "../../app-context";
import { Drawer } from "../../components/Drawer";
import { Field, SecretInput } from "../../components/fields";
import { t, tx } from "../../i18n";
import { api, ApiError, errorMessage } from "../../lib/api";
import type { ClusterSetupRequest } from "../../lib/types";

export function ClusterSetup({
  available,
  onComplete,
}: {
  available: boolean;
  onComplete: () => void;
}) {
  const [mode, setMode] = useState<"create" | "join" | null>(null);
  const app = useAppOptional();
  const configured = app?.boot.setupComplete ?? true;
  return (
    <>
      <div className="grid gap-4 md:grid-cols-2">
        <div className="space-y-4 rounded-xl border border-border bg-surface p-6">
          <Database size={24} className="text-primary" />
          <div>
            <h2 className="text-base font-semibold text-fg">{t("Create a cluster")}</h2>
            <p className="mt-1 text-sm text-fg-muted">
              {t(
                "Make this server the first node. Ether backs up your data and moves your settings, viewers and history to PostgreSQL.",
              )}
            </p>
          </div>
          <Button disabled={!available || !configured} onClick={() => setMode("create")}>
            {t("Create cluster")}
          </Button>
          {!configured ? (
            <p className="text-xs text-fg-muted">
              {tx("{finishSetup} before creating a cluster.", {
                finishSetup: (
                  <Link className="underline" to="/setup">
                    {t("Finish this server’s setup")}
                  </Link>
                ),
              })}
            </p>
          ) : null}
        </div>
        <div className="space-y-4 rounded-xl border border-border bg-surface p-6">
          <Network size={24} className="text-primary" />
          <div>
            <h2 className="text-base font-semibold text-fg">{t("Join an existing cluster")}</h2>
            <p className="mt-1 text-sm text-fg-muted">
              {t(
                "Add this machine using a one-time token from another node. It adopts the cluster’s server identity and shared settings.",
              )}
            </p>
          </div>
          <Button variant="outline" disabled={!available} onClick={() => setMode("join")}>
            {t("Join cluster")}
          </Button>
        </div>
      </div>
      {!available ? (
        <Alert variant="warning">
          <AlertDescription>
            {t(
              "Guided setup is unavailable because this deployment controls its database through the environment.",
            )}
          </AlertDescription>
        </Alert>
      ) : null}
      <p className="text-sm text-fg-muted">
        {t(
          "You supply a PostgreSQL 14+ database reachable by every node. Ether handles the backup, migration and restart automatically. Database hosting and availability are managed separately.",
        )}
      </p>
      {mode ? (
        <SetupWizard
          mode={mode}
          configured={configured}
          onClose={() => setMode(null)}
          onComplete={onComplete}
        />
      ) : null}
    </>
  );
}

function SetupWizard({
  mode,
  configured,
  onClose,
  onComplete,
}: {
  mode: "create" | "join";
  configured: boolean;
  onClose: () => void;
  onComplete: () => void;
}) {
  const [step, setStep] = useState(0);
  const [host, setHost] = useState("");
  const [port, setPort] = useState("5432");
  const [database, setDatabase] = useState("ether");
  const [user, setUser] = useState("ether");
  const [password, setPassword] = useState("");
  const [tls, setTLS] = useState("verify-full");
  const [token, setToken] = useState("");
  const [override, setOverride] = useState("");
  const [replace, setReplace] = useState(false);
  const [busy, setBusy] = useState(false);
  const [waiting, setWaiting] = useState(false);
  const [error, setError] = useState("");
  const [message, setMessage] = useState(() => t("Waiting for Ether to reconnect…"));
  const create = mode === "create";
  const databaseURL = `postgresql://${encodeURIComponent(user)}:${encodeURIComponent(password)}@${host.includes(":") && !host.startsWith("[") ? `[${host}]` : host}:${port}/${encodeURIComponent(database)}?sslmode=${tls}`;
  const request: ClusterSetupRequest = {
    mode,
    databaseUrl: create ? databaseURL : override.trim(),
    token: create ? "" : token.trim(),
    replace,
  };
  const valid = create
    ? Boolean(
        host.trim() &&
        user.trim() &&
        database.trim() &&
        Number.isInteger(Number(port)) &&
        Number(port) >= 1 &&
        Number(port) <= 65535,
      )
    : Boolean(token.trim());

  useEffect(() => {
    if (!waiting) return;
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout>;
    const poll = async () => {
      try {
        const state = await api.clusterSetupStatus();
        if (cancelled) return;
        if (state.state === "complete") {
          onComplete();
          onClose();
          return;
        }
        if (state.state === "idle") {
          setWaiting(false);
          setStep(0);
          setError(t("No setup is pending. Check the connection and try again."));
          return;
        }
        if (state.state === "failed") {
          setWaiting(false);
          setStep(0);
          setError(state.message || t("Cluster setup failed. Try again."));
          return;
        }
        if (state.message) setMessage(state.message);
      } catch (e) {
        // A joined server now requires the cluster owner's credential.
        if (!cancelled && e instanceof ApiError && (e.status === 401 || e.status === 403)) {
          window.location.reload();
          return;
        }
      }
      if (!cancelled) timer = setTimeout(poll, 2000);
    };
    void poll();
    return () => {
      cancelled = true;
      clearTimeout(timer);
    };
  }, [waiting, onClose, onComplete]);

  const check = async () => {
    setBusy(true);
    setError("");
    try {
      await api.checkClusterSetup(request);
      setStep(1);
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  const apply = async () => {
    setBusy(true);
    setError("");
    try {
      await api.applyClusterSetup(request);
      setPassword("");
      setToken("");
      setOverride("");
      setWaiting(true);
      setStep(2);
    } catch (e) {
      // A response can be lost as the listener restarts; reconnect before retrying.
      if (e instanceof ApiError && e.status === 0) {
        setWaiting(true);
        setStep(2);
      } else setError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Drawer
      open
      onClose={() => {
        if (!busy && !waiting) onClose();
      }}
      title={create ? t("Create cluster") : t("Join cluster")}
      description={t("Set up this node without leaving Server Manager.")}
      size="lg"
      footer={
        waiting ? (
          <Button variant="outline" onClick={() => window.location.reload()}>
            {t("Reconnect")}
          </Button>
        ) : (
          <div className="flex w-full items-center justify-between gap-2">
            <Button
              variant="outline"
              disabled={busy}
              onClick={() => (step ? setStep(0) : onClose())}
            >
              {step ? t("Back") : t("Cancel")}
            </Button>
            <Button
              disabled={busy || !valid || (step === 1 && !create && configured && !replace)}
              onClick={() => void (step === 0 ? check() : apply())}
            >
              {busy
                ? step === 0
                  ? t("Checking…")
                  : t("Preparing…")
                : step === 0
                  ? t("Test connection and continue")
                  : create
                    ? t("Create cluster and restart")
                    : t("Join cluster and restart")}
            </Button>
          </div>
        )
      }
    >
      <div className="space-y-6">
        <ol aria-label={t("Cluster setup progress")} className="grid grid-cols-3 gap-2 text-xs">
          {[t("Connect"), t("Review"), t("Start")].map((label, index) => (
            <li
              key={index}
              aria-current={step === index ? "step" : undefined}
              className={`flex items-center gap-2 border-t-2 pt-3 ${step >= index ? "border-primary text-fg" : "border-border text-fg-muted"}`}
            >
              <span className="grid size-5 place-items-center rounded-full bg-bg-subtle">
                {step > index ? <Check size={12} /> : index + 1}
              </span>
              {label}
            </li>
          ))}
        </ol>
        {error ? (
          <Alert variant="destructive" role="alert">
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        ) : null}
        {step === 0 ? (
          create ? (
            <>
              <div>
                <h3 className="font-medium text-fg">{t("Connect your database")}</h3>
                <p className="mt-1 text-sm text-fg-muted">
                  {t(
                    "Use an empty PostgreSQL database. The connection is tested from this Ether server.",
                  )}
                </p>
              </div>
              <div className="grid grid-cols-[1fr_6rem] gap-3">
                <Field label={t("Database host")} htmlFor="cluster-host">
                  <Input
                    id="cluster-host"
                    value={host}
                    placeholder="db.lan"
                    onChange={(e) => setHost(e.target.value.trim())}
                  />
                </Field>
                <Field label={t("Port")} htmlFor="cluster-port">
                  <Input
                    id="cluster-port"
                    type="number"
                    min={1}
                    max={65535}
                    value={port}
                    onChange={(e) => setPort(e.target.value)}
                  />
                </Field>
              </div>
              <Field label={t("Database name")} htmlFor="cluster-db">
                <Input
                  id="cluster-db"
                  value={database}
                  onChange={(e) => setDatabase(e.target.value)}
                />
              </Field>
              <Field label={t("Database user")} htmlFor="cluster-user">
                <Input
                  id="cluster-user"
                  value={user}
                  onChange={(e) => setUser(e.target.value)}
                  autoComplete="off"
                />
              </Field>
              <Field label={t("Database password")} htmlFor="cluster-password">
                <SecretInput id="cluster-password" value={password} onChange={setPassword} />
              </Field>
              <Field label={t("Database encryption")} htmlFor="cluster-tls">
                <select
                  id="cluster-tls"
                  className="w-full rounded-md border border-border bg-surface px-3 py-2 text-sm"
                  value={tls}
                  onChange={(e) => setTLS(e.target.value)}
                >
                  <option value="verify-full">{t("Verify TLS certificate and hostname")}</option>
                  <option value="require">{t("TLS without certificate verification")}</option>
                  <option value="disable">{t("No TLS (credentials are sent unencrypted)")}</option>
                </select>
                {tls !== "verify-full" ? (
                  <p className="mt-2 text-sm text-warning" role="alert">
                    {tls === "disable"
                      ? t("Database traffic and credentials will be sent without encryption.")
                      : t(
                          "TLS encrypts traffic, but this mode does not verify the database server’s identity.",
                        )}
                  </p>
                ) : null}
              </Field>
            </>
          ) : (
            <>
              <div>
                <h3 className="font-medium text-fg">{t("Connect to your cluster")}</h3>
                <p className="mt-1 text-sm text-fg-muted">
                  {t(
                    "On an existing node, open Cluster → Create join token. Copy the token here. It includes the database connection and works once.",
                  )}
                </p>
              </div>
              <Field label={t("Join token")} htmlFor="cluster-token">
                <SecretInput id="cluster-token" value={token} onChange={setToken} />
              </Field>
              <Field
                label={t("Database URL override (optional)")}
                htmlFor="cluster-override"
                hint={t("Only needed if this node reaches PostgreSQL at a different address.")}
              >
                <SecretInput id="cluster-override" value={override} onChange={setOverride} />
              </Field>
              {configured ? (
                <label className="flex items-start gap-3 text-sm">
                  <Checkbox checked={replace} onChange={(e) => setReplace(e.target.checked)} />
                  <span>
                    {t(
                      "Replace this server’s identity and shared settings with the cluster’s. Keep its local data in an automatic backup.",
                    )}
                  </span>
                </label>
              ) : null}
            </>
          )
        ) : step === 1 ? (
          <>
            <Alert variant="success">
              <AlertTitle>{t("Connection verified")}</AlertTitle>
              <AlertDescription>
                {create
                  ? t("The database is reachable and empty.")
                  : t("The cluster is reachable and the join token is valid.")}
              </AlertDescription>
            </Alert>
            <h3 className="font-medium text-fg">
              {create ? t("Ready to create your cluster") : t("Ready to join this cluster")}
            </h3>
            <ol className="list-decimal space-y-3 pl-5 text-sm text-fg-muted">
              <li>
                {t(
                  "Create a portable backup of this server’s database, settings and credential key.",
                )}
              </li>
              <li>
                {t(
                  "Stop this server and its background work safely. Active streams must finish before setup can proceed.",
                )}
              </li>
              <li>
                {create
                  ? t("Move your data to {database} on {host}. Keep the same server identity.", {
                      database,
                      host,
                    })
                  : t(
                      "Adopt the cluster’s identity, viewers and settings. This server’s existing history is not merged.",
                    )}
              </li>
              <li>
                {t("Restart Ether automatically and reconnect this page.")}{" "}
                {create
                  ? t("Then create a join token to add your next machine.")
                  : t("Sign in with the cluster owner’s Plex account if prompted.")}
              </li>
            </ol>
            <p className="text-xs text-fg-muted">
              {t(
                "Local port, cache paths and transcoding settings stay on this machine. The original SQLite database stays in your data directory.",
              )}
            </p>
          </>
        ) : (
          <div className="space-y-4 py-8 text-center" role="status">
            <Server size={36} className="mx-auto animate-pulse text-primary" />
            <h3 className="text-base font-semibold text-fg">{t("Starting your cluster node")}</h3>
            <p className="text-sm text-fg-muted">{message}</p>
            <p className="text-xs text-fg-muted">
              {t(
                "The connection may be unavailable while data is copied. This page reconnects automatically.",
              )}
            </p>
          </div>
        )}
      </div>
    </Drawer>
  );
}
