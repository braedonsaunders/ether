import {
  Alert,
  AlertDescription,
  AlertTitle,
  Badge,
  Button,
  confirmDialog,
  EmptyState,
  Input,
  PagedTable,
  SettingsSection,
  Textarea,
  toast,
} from "@braedonsaunders/appkit-ui";
import { Copy, KeyRound, Plus } from "lucide-react";
import { useState } from "react";
import { Link } from "react-router-dom";
import { MoreOptions } from "../../components/ConfigSection";
import { Drawer } from "../../components/Drawer";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { Field, Segmented } from "../../components/fields";
import { ClusterSetup } from "./ClusterSetup";
import { Section } from "../../components/layout";
import { SettingsRow } from "../../components/SettingsRow";
import { pagedTableLabels } from "../../i18n/appkit-labels";
import { localized, t, tn, tx } from "../../i18n";
import { api, errorMessage } from "../../lib/api";
import { formatDateTime, parseTime, relativeTime } from "../../lib/format";
import { useAsync, useInterval, type AsyncState } from "../../lib/hooks";
import type {
  ClusterNode,
  ClusterSettings,
  ClusterStatus,
  CreatedJoinToken,
  JoinTokenInfo,
} from "../../lib/types";

export const CLUSTER_DOCS =
  "https://github.com/braedonsaunders/ether/blob/main/docs/install.md#running-a-cluster";

/** How often the page refreshes membership and tokens while it is visible. */
export const CLUSTER_POLL_MS = 10_000;

/** A node is live within 20 s of its last heartbeat; older means the database is unreachable. */
const HEARTBEAT_STALE_MS = 20_000;
/** Clock differences beyond this confuse liveness and token expiry. */
const SKEW_WARN_NS = 2e9;

export function isClustered(s: ClusterStatus | null | undefined): boolean {
  return s?.mode === "postgres";
}

/**
 * Whether this server is a cluster node, read once per page; false while
 * loading and when the server doesn't report it (standalone behaviour).
 */
export function useClustered(): boolean {
  const status = useAsync((signal) => api.clusterStatus(signal).catch(() => null), []);
  return isClustered(status.data);
}

function nodeName(n: ClusterNode): string {
  return n.name || n.id;
}

/** "videotoolbox", "Software" or "Unavailable" (no ffmpeg on the node). */
export function transcodeLabel(n: ClusterNode): string {
  if (!n.caps?.transcode) return t("Unavailable");
  const hw = n.caps.hwAccel;
  return hw && hw !== "none" ? hw : t("Software");
}

/** Server warnings plus conditions this node's own health reports. */
export function clusterWarnings(s: ClusterStatus, now: Date = new Date()): string[] {
  // Server warnings are log-style sentences; capitalise them for the banner.
  const out = (s.warnings ?? []).map((w) => w.charAt(0).toUpperCase() + w.slice(1));
  if (s.error)
    out.push(t("Couldn't read the cluster's nodes from the database: {error}", { error: s.error }));
  const h = s.health;
  if (h) {
    const beat = parseTime(h.lastHeartbeat);
    if (!beat || now.getTime() - beat.getTime() > HEARTBEAT_STALE_MS)
      out.push(
        beat
          ? t(
              "This node hasn't reached the database since {when}. Streams keep playing, but other nodes may treat it as offline.",
              { when: relativeTime(beat, now) },
            )
          : t("This node hasn't reached the database yet."),
      );
    else if (!h.busConnected)
      out.push(
        t(
          "This node's event connection to the database is down, so changes made on other nodes may reach it late.",
        ),
      );
    if (Math.abs(h.clockSkewNs ?? 0) > SKEW_WARN_NS)
      out.push(
        tn(
          Math.round(Math.abs(h.clockSkewNs) / 1e9),
          "This node's clock is {count} second off the database server's. Sync the clock (NTP) on every node.",
          "This node's clock is {count} seconds off the database server's. Sync the clock (NTP) on every node.",
        ),
      );
  }
  return out;
}

export type TokenState = "used" | "expired" | "unused";

export function tokenState(token: JoinTokenInfo, now: Date = new Date()): TokenState {
  if (parseTime(token.usedAt)) return "used";
  const exp = parseTime(token.expiresAt);
  if (exp && exp.getTime() <= now.getTime()) return "expired";
  return "unused";
}

/** Live nodes first, the leader first among them, then by name. */
function sortNodes(nodes: ClusterNode[]): ClusterNode[] {
  return [...nodes].sort(
    (a, b) =>
      Number(b.live) - Number(a.live) ||
      Number(b.leader) - Number(a.leader) ||
      nodeName(a).localeCompare(nodeName(b)),
  );
}

function copyToken(text: string) {
  void navigator.clipboard
    .writeText(text)
    .then(() => toast.success(t("Token copied")))
    .catch(() => toast.error(t("Select and copy the token")));
}

/**
 * Server › Cluster: on a standalone server, what clustering is and how to
 * start; on a cluster node, the nodes, their health and the join tokens.
 */
export function ClusterOverview() {
  const status = useAsync((signal) => api.clusterStatus(signal), []);
  const clustered = isClustered(status.data);
  const [addingNode, setAddingNode] = useState(false);
  const setup = useAsync((signal) => api.clusterSetupStatus(signal), []);
  const settings = useAsync(
    async (signal) => (clustered ? api.clusterSettings(signal) : null),
    [clustered],
  );
  useInterval(() => {
    void status.reload();
    if (clustered) void settings.reload();
  }, CLUSTER_POLL_MS);

  if (status.error && !status.data)
    return <ErrorPanel error={status.error} onRetry={() => void status.reload()} />;
  const s = status.data;
  if (!s) return <LoadingRows rows={3} />;
  if (!clustered)
    return (
      <Standalone
        status={s}
        available={setup.data?.available ?? false}
        message={setup.data?.state === "failed" ? setup.data.message : undefined}
        onComplete={() => {
          void status.reload();
          void setup.reload();
        }}
      />
    );

  const warnings = clusterWarnings(s);
  return (
    <>
      {warnings.length ? (
        <Alert variant="warning" role="status">
          <AlertTitle>{t("Cluster needs attention")}</AlertTitle>
          <AlertDescription>
            <ul className="list-disc space-y-1 pl-5">
              {warnings.map((w) => (
                <li key={w}>{w}</li>
              ))}
            </ul>
          </AlertDescription>
        </Alert>
      ) : null}
      <Nodes status={s} onChanged={() => void status.reload()} onAdd={() => setAddingNode(true)} />
      <SharedSettings status={s} settings={settings} />
      <JoinTokens settings={settings} />
      {addingNode ? (
        <JoinTokenDrawer
          onClose={() => setAddingNode(false)}
          onCreated={() => void settings.reload()}
        />
      ) : null}
    </>
  );
}

function Standalone({
  status,
  available,
  message,
  onComplete,
}: {
  status: ClusterStatus;
  available: boolean;
  message?: string;
  onComplete: () => void;
}) {
  return (
    <>
      {message ? (
        <Alert variant="destructive" role="alert">
          <AlertTitle>{t("Cluster setup needs attention")}</AlertTitle>
          <AlertDescription>{message}</AlertDescription>
        </Alert>
      ) : null}
      <SettingsSection title={t("This server")}>
        <SettingsRow
          title={t("Role")}
          description={t("Your data currently lives in this server’s SQLite database.")}
          control={<Badge variant="secondary">{t("Standalone (SQLite)")}</Badge>}
        />
        {status.self?.version ? (
          <SettingsRow title={t("Version")} description={status.self.version} />
        ) : null}
      </SettingsSection>
      <ClusterSetup available={available} onComplete={onComplete} />
      <a
        className="text-sm text-fg-muted underline"
        href={CLUSTER_DOCS}
        target="_blank"
        rel="noreferrer"
      >
        {t("Read the cluster guide")}
      </a>
    </>
  );
}

export function managerURL(n: ClusterNode): string | null {
  for (const addr of n.addrs ?? []) {
    try {
      const u = new URL(addr);
      if ((u.protocol === "http:" || u.protocol === "https:") && !u.username && !u.password)
        return new URL("/ether/cluster", u).href;
    } catch {
      /* skip malformed connections */
    }
  }
  return null;
}

function Nodes({
  status,
  onChanged,
  onAdd,
}: {
  status: ClusterStatus;
  onChanged: () => void;
  onAdd: () => void;
}) {
  const [manage, setManage] = useState(false);
  const nodes = sortNodes(status.nodes ?? []);
  const live = nodes.filter((n) => n.live).length;
  return (
    <Section
      title={t("Nodes")}
      description={tn(
        nodes.length,
        "{live} of {count} node online. Open a node’s manager to change its local settings.",
        "{live} of {count} nodes online. Open a node’s manager to change its local settings.",
        { live },
      )}
      actions={
        <div className="flex gap-2">
          <Button size="sm" onClick={onAdd}>
            <Plus size={14} /> {t("Add node")}
          </Button>
          <Button size="sm" variant="outline" onClick={() => setManage(true)}>
            {t("Manage this node")}
          </Button>
        </div>
      }
    >
      <PagedTable<ClusterNode>
        rows={nodes}
        rowKey={(n) => n.id}
        pageSize={25}
        searchable={nodes.length > 8}
        labels={pagedTableLabels()}
        empty={
          <EmptyState
            title={t("No nodes reported")}
            description={t("The database couldn't be read. Nodes appear here once it can.")}
          />
        }
        columns={[
          {
            key: "name",
            header: t("Node"),
            search: (n) => `${nodeName(n)} ${n.version}`,
            cell: (n) => (
              <div className="min-w-0">
                <div className="flex flex-wrap items-center gap-1.5">
                  <span className="font-medium text-fg">{nodeName(n)}</span>
                  {n.leader ? <Badge variant="info">{t("Leader")}</Badge> : null}
                  {n.id === status.self?.id ? (
                    <Badge variant="outline">{t("This node")}</Badge>
                  ) : null}
                </div>
                <span className="block text-xs text-fg-muted">
                  {n.version
                    ? t("Version {version}", { version: n.version })
                    : t("Version unknown")}
                </span>
              </div>
            ),
          },
          {
            key: "status",
            header: t("Status"),
            cell: (n) =>
              n.live && n.draining ? (
                <Badge variant="warning">{t("Paused")}</Badge>
              ) : n.live ? (
                <Badge variant="success">{t("Online")}</Badge>
              ) : (
                <span className="flex flex-col items-start gap-0.5">
                  <Badge variant="secondary">{t("Offline")}</Badge>
                  <span className="text-xs text-fg-muted" title={formatDateTime(n.lastSeen)}>
                    {t("Last seen {when}", { when: relativeTime(n.lastSeen) })}
                  </span>
                </span>
              ),
          },
          {
            key: "load",
            header: t("Streams"),
            align: "right",
            cell: (n) => (
              <span
                className="tabular-nums"
                aria-label={t("{node} streams", { node: nodeName(n) })}
              >
                {n.live ? n.load : "—"}
              </span>
            ),
          },
          {
            key: "transcode",
            header: t("Transcoding"),
            cell: (n) => <span className="text-fg-muted">{transcodeLabel(n)}</span>,
          },
          {
            key: "weight",
            header: t("Weight"),
            align: "right",
            cell: (n) => (
              <span className="tabular-nums" aria-label={t("{node} weight", { node: nodeName(n) })}>
                {n.caps?.weight || 1}
              </span>
            ),
          },
          {
            key: "manage",
            header: "",
            cell: (n) =>
              n.id === status.self.id ? null : managerURL(n) ? (
                <a
                  className="text-sm text-primary underline"
                  href={managerURL(n)!}
                  target="_blank"
                  rel="noreferrer"
                  aria-label={t("Open manager for {node}", { node: nodeName(n) })}
                >
                  {t("Open manager")}
                </a>
              ) : (
                <span className="text-xs text-fg-muted">{t("No address reported")}</span>
              ),
          },
          {
            key: "ip",
            header: t("Public IP"),
            cell: (n) => <code className="text-xs text-fg-muted">{n.caps?.egressIp || "—"}</code>,
          },
        ]}
      />
      {manage ? (
        <NodeDrawer status={status} onClose={() => setManage(false)} onChanged={onChanged} />
      ) : null}
    </Section>
  );
}

function NodeDrawer({
  status,
  onClose,
  onChanged,
}: {
  status: ClusterStatus;
  onClose: () => void;
  onChanged: () => void;
}) {
  const [weight, setWeight] = useState(status.self.caps.weight || 1);
  const [draining, setDraining] = useState(status.self.draining ?? false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const save = async (body: { weight?: number; draining?: boolean }) => {
    setBusy(true);
    setError("");
    try {
      const result = await api.updateClusterNode(body);
      setDraining(result.draining);
      onChanged();
      toast.success(t("Node updated"));
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Drawer
      open
      onClose={onClose}
      title={t("Manage this node")}
      description={nodeName(status.self)}
      footer={
        <Button variant="outline" onClick={onClose}>
          {t("Done")}
        </Button>
      }
    >
      <div className="space-y-6">
        {error ? (
          <Alert variant="destructive" role="alert">
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        ) : null}
        <Field
          label={t("Node weight")}
          htmlFor="cluster-weight"
          hint={t(
            "This computer's share of each provider's connections and request limits, relative to the others. 2 gets twice the share of a computer at 1; 0 means 1.",
          )}
        >
          <Input
            id="cluster-weight"
            type="number"
            min={1}
            max={100}
            value={weight}
            onChange={(e) => setWeight(e.target.valueAsNumber)}
          />
        </Field>
        <Button
          disabled={busy || !Number.isInteger(weight) || weight < 1 || weight > 100}
          onClick={() => void save({ weight })}
        >
          {t("Save weight")}
        </Button>
        <SettingsSection title={t("New streams")}>
          <SettingsRow
            title={draining ? t("Paused") : t("Accepting streams")}
            description={t(
              "Pausing keeps current streams playing, hands scheduled work to another node and removes this node from published connections. It resumes automatically after a server restart.",
            )}
          />
        </SettingsSection>
        <Alert variant="warning">
          <AlertDescription>
            {t("Pausing the only available node prevents new playback until you resume it.")}
          </AlertDescription>
        </Alert>
        <Button
          variant="outline"
          disabled={busy}
          onClick={() => void save({ draining: !draining })}
        >
          {draining ? t("Resume new streams") : t("Pause new streams")}
        </Button>
        <p className="text-sm text-fg-muted">
          {tx(
            "Change this node’s port, cache and transcoding hardware from {general}. Use {updates} for rolling cluster updates.",
            {
              general: (
                <Link className="underline" to="/server">
                  {t("General settings")}
                </Link>
              ),
              updates: (
                <Link className="underline" to="/maintenance/updates">
                  {t("Updates")}
                </Link>
              ),
            },
          )}
        </p>
      </div>
    </Drawer>
  );
}

type SettingsState = AsyncState<ClusterSettings | null>;

function SharedSettings({ status, settings }: { status: ClusterStatus; settings: SettingsState }) {
  const st = settings.data;
  const self = status.self;
  return (
    <SettingsSection title={t("Shared settings")}>
      <SettingsRow
        title={t("This node")}
        description={
          self?.id === status.leader
            ? t("Leads the cluster: it publishes the server to plex.tv and runs scheduled work.")
            : t("A cluster member. Scheduled work runs on the leader.")
        }
        control={<Badge variant="info">{nodeName(self)}</Badge>}
      />
      <MoreOptions label={t("More options: {title}", { title: t("Shared settings") })}>
        <SettingsRow
          title={t("Settings version")}
          description={t(
            "Settings are stored in the database. A change on any node reaches the others within 30 seconds.",
          )}
          control={
            st?.sharedVersion !== undefined ? (
              <span className="tabular-nums text-sm text-fg">v{st.sharedVersion}</span>
            ) : null
          }
        />
        <SettingsRow
          title={t("Credential key")}
          description={t(
            "Fingerprint of the key that encrypts stored credentials. It's the same on every node.",
          )}
          control={st?.keyFingerprint ? <code className="text-xs">{st.keyFingerprint}</code> : null}
        />
      </MoreOptions>
      <SettingsRow
        title={t("Settings each node keeps")}
        description={tx(
          "Port, listen address, log level, cache, transcoding hardware, the public port for its port forward and the node's weight are set on each node, on that node's own {general} page.",
          {
            general: (
              <Link to="/server" className="underline">
                {t("General")}
              </Link>
            ),
          },
        )}
      />
      {settings.error ? (
        <div className="px-5 pb-4">
          <ErrorPanel error={settings.error} onRetry={() => void settings.reload()} />
        </div>
      ) : null}
    </SettingsSection>
  );
}

const TOKEN_STATE: Record<
  TokenState,
  { label: string; variant: "success" | "secondary" | "warning" }
> = localized(() => ({
  unused: { label: t("Unused"), variant: "success" },
  used: { label: t("Used"), variant: "secondary" },
  expired: { label: t("Expired"), variant: "warning" },
}));

function JoinTokens({ settings }: { settings: SettingsState }) {
  const [open, setOpen] = useState(false);
  const [busy, setBusy] = useState<string | null>(null);
  const tokens = settings.data?.joinTokens ?? [];

  const revoke = async (token: JoinTokenInfo) => {
    const ok = await confirmDialog({
      title: t("Revoke join token?"),
      message: t(
        "Token {id} stops working right away. Nodes that already joined are not affected.",
        { id: token.id },
      ),
      confirmLabel: t("Revoke"),
      tone: "danger",
    });
    if (!ok) return;
    setBusy(token.id);
    try {
      await api.revokeJoinToken(token.id);
      await settings.reload();
      toast.success(t("Join token revoked"));
    } catch (e) {
      toast.error(t("Couldn't revoke the token"), { description: errorMessage(e) });
    } finally {
      setBusy(null);
    }
  };

  return (
    <Section
      title={t("Join tokens")}
      description={t(
        "A new node joins with a one-time token created here. Each token works once and expires within the hour.",
      )}
      actions={
        <Button size="sm" onClick={() => setOpen(true)}>
          <Plus size={14} /> {t("Create join token")}
        </Button>
      }
    >
      {!settings.data && !settings.error ? (
        <LoadingRows rows={2} />
      ) : (
        <PagedTable<JoinTokenInfo>
          rows={tokens}
          rowKey={(token) => token.id}
          pageSize={10}
          searchable={false}
          labels={pagedTableLabels()}
          empty={
            <EmptyState
              icon={<KeyRound />}
              title={t("No join tokens")}
              description={t("Create one when you're ready to add a node.")}
            />
          }
          columns={[
            {
              key: "id",
              header: t("Token"),
              cell: (token) => <code className="text-xs">{token.id}</code>,
            },
            {
              key: "created",
              header: t("Created"),
              cell: (token) => (
                <span className="text-fg-muted" title={formatDateTime(token.createdAt)}>
                  {relativeTime(token.createdAt)}
                </span>
              ),
            },
            {
              key: "expires",
              header: t("Expires"),
              cell: (token) => (
                <span className="text-fg-muted" title={formatDateTime(token.expiresAt)}>
                  {tokenState(token) === "used" ? "—" : relativeTime(token.expiresAt)}
                </span>
              ),
            },
            {
              key: "state",
              header: t("Status"),
              cell: (token) => {
                const state = tokenState(token);
                return (
                  <span className="flex flex-col items-start gap-0.5">
                    <Badge variant={TOKEN_STATE[state].variant}>{TOKEN_STATE[state].label}</Badge>
                    {state === "used" ? (
                      <span className="text-xs text-fg-muted" title={formatDateTime(token.usedAt)}>
                        {token.usedBy
                          ? t("by {node}, {when}", {
                              node: token.usedBy,
                              when: relativeTime(token.usedAt),
                            })
                          : relativeTime(token.usedAt)}
                      </span>
                    ) : null}
                  </span>
                );
              },
            },
            {
              key: "actions",
              header: "",
              align: "right",
              cell: (token) =>
                tokenState(token) === "used" ? null : (
                  <Button
                    variant="outline"
                    size="sm"
                    disabled={busy === token.id}
                    aria-label={t("Revoke token {id}", { id: token.id })}
                    onClick={() => void revoke(token)}
                  >
                    {t("Revoke")}
                  </Button>
                ),
            },
          ]}
        />
      )}
      {open ? (
        <JoinTokenDrawer onClose={() => setOpen(false)} onCreated={() => void settings.reload()} />
      ) : null}
    </Section>
  );
}

const TTL_OPTIONS = localized(() => [
  { value: "15", label: tn(15, "{count} minute", "{count} minutes") },
  { value: "30", label: tn(30, "{count} minute", "{count} minutes") },
  { value: "60", label: tn(1, "{count} hour", "{count} hours") },
]);

function JoinTokenDrawer({ onClose, onCreated }: { onClose: () => void; onCreated: () => void }) {
  const [ttl, setTtl] = useState("60");
  const [busy, setBusy] = useState(false);
  // Held only while the drawer is open: the server never returns it again.
  const [created, setCreated] = useState<CreatedJoinToken | null>(null);

  const create = async () => {
    setBusy(true);
    try {
      setCreated(await api.createJoinToken(Number(ttl)));
      onCreated();
    } catch (e) {
      toast.error(t("Couldn't create a join token"), { description: errorMessage(e) });
    } finally {
      setBusy(false);
    }
  };

  return (
    <Drawer
      open
      onClose={onClose}
      title={t("Create join token")}
      description={t("Lets one new machine join this cluster.")}
      size="lg"
      footer={
        created ? (
          <Button variant="outline" onClick={onClose}>
            {t("Done")}
          </Button>
        ) : (
          <Button disabled={busy} onClick={() => void create()}>
            {busy ? t("Creating…") : t("Create join token")}
          </Button>
        )
      }
    >
      <div className="space-y-5">
        <Field
          label={t("Valid for")}
          hint={t("Use the token before it expires; you can always create another.")}
        >
          {created ? (
            <p className="text-sm text-fg">
              {t("Expires {when}", { when: formatDateTime(created.expiresAt) })}
            </p>
          ) : (
            <Segmented
              ariaLabel={t("Valid for")}
              options={TTL_OPTIONS}
              value={ttl}
              onChange={setTtl}
            />
          )}
        </Field>
        {created ? (
          <>
            <Alert variant="warning" role="alert">
              <AlertTitle>{t("Keep this token secret")}</AlertTitle>
              <AlertDescription>
                {created.warning ||
                  t(
                    "It contains the database password and the key that decrypts every stored credential.",
                  )}{" "}
                {t("It's shown only once: after you close this drawer it can't be shown again.")}
              </AlertDescription>
            </Alert>
            <Field label={t("Join token")}>
              <Textarea
                aria-label={t("Join token")}
                readOnly
                rows={4}
                value={created.token}
                className="font-mono text-xs break-all"
              />
            </Field>
            <Button variant="outline" size="sm" onClick={() => copyToken(created.token)}>
              <Copy size={14} /> {t("Copy token")}
            </Button>
            <Field
              label={t("On the new machine")}
              hint={t(
                "Open its Server Manager, choose Cluster → Join cluster and paste this token. Ether checks the connection, backs up its data and restarts automatically.",
              )}
            >
              <p className="text-sm text-fg-muted">
                {t(
                  "Install and open Ether on the new machine. You can join before completing its standalone setup.",
                )}
              </p>
            </Field>
          </>
        ) : (
          <p className="text-sm text-fg-muted">
            {t(
              "Paste the token into Cluster → Join cluster on the new machine. It is shown once and contains the database password and the credential key, so treat it like a password.",
            )}
          </p>
        )}
      </div>
    </Drawer>
  );
}
