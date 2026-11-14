import { Badge, Button, Checkbox, Input, Switch, cn } from "@braedonsaunders/appkit-ui";
import { ArrowDown, ArrowUp, ArrowUpToLine, Search, ShieldCheck } from "lucide-react";
import { memo, useCallback, useMemo, useState } from "react";
import { DragHandle, SortableList } from "../../components/Sortable";
import { Segmented } from "../../components/fields";
import { t, tn } from "../../i18n";
import { formatDateTime, formatNumber, relativeTime } from "../../lib/format";
import { normGroup, type GroupRules, type SourceAccount, type SourceGroup } from "./model";

const DAY = 24 * 60 * 60 * 1000;

function accountStatus(status: string | undefined): {
  label: string;
  tone: "success" | "destructive" | "secondary";
} | null {
  const s = status?.trim();
  if (!s) return null;
  switch (s.toLowerCase()) {
    case "active":
      return { label: t("Active"), tone: "success" };
    case "expired":
      return { label: t("Expired"), tone: "destructive" };
    case "banned":
      return { label: t("Banned"), tone: "destructive" };
    case "disabled":
      return { label: t("Disabled"), tone: "destructive" };
    default:
      return { label: s, tone: "secondary" };
  }
}

/** What the provider's panel said about this login at the last import. Read-only. */
export function ProviderAccountCard({ account }: { account: SourceAccount }) {
  const status = accountStatus(account.status);
  const expires = account.expiresAt ? Date.parse(account.expiresAt) : NaN;
  const left = Number.isNaN(expires) ? null : expires - Date.now();
  const max = account.maxConnections ?? 0;
  return (
    <section
      aria-label={t("Provider account")}
      className="space-y-2 rounded-xl border border-border bg-surface px-4 py-3 text-sm"
    >
      <div className="flex flex-wrap items-center gap-2">
        <ShieldCheck size={15} className="shrink-0 text-fg-subtle" aria-hidden />
        <span className="font-medium text-fg">{t("Provider account")}</span>
        {status ? <Badge variant={status.tone}>{status.label}</Badge> : null}
        {account.trial ? <Badge variant="info">{t("Trial")}</Badge> : null}
        <span className="ml-auto text-xs text-fg-subtle">
          {t("Checked {when}", { when: relativeTime(account.checkedAt) })}
        </span>
      </div>
      <p className="text-xs text-fg-muted">
        {t("Connections")}: {account.activeConnections} / {max || 1}
      </p>
      {left !== null ? (
        <p
          className={cn(
            "text-sm",
            left <= 0 ? "text-danger" : left < DAY ? "font-medium text-warning" : "text-fg-muted",
          )}
        >
          {left <= 0
            ? t("Expired {date} ({when})", {
                date: formatDateTime(account.expiresAt),
                when: relativeTime(account.expiresAt),
              })
            : t("Expires {date} ({when})", {
                date: formatDateTime(account.expiresAt),
                when: relativeTime(account.expiresAt),
              })}
        </p>
      ) : null}
      {max > 0 ? (
        <div>
          <p className="font-medium text-fg">
            {tn(max, "Allows {count} connection", "Allows {count} connections")}
          </p>
          <p className="text-xs text-fg-muted">
            {tn(
              max,
              "Ether never opens more than {count} stream to this provider at once, and viewers watching the same channel share one stream.",
              "Ether never opens more than {count} streams to this provider at once, and viewers watching the same channel share one stream.",
            )}
          </p>
        </div>
      ) : null}
    </section>
  );
}

type Rules = Required<Pick<GroupRules, "excludeGroups" | "groupOrder">> &
  Pick<GroupRules, "newGroups" | "hideAdult">;

const read = (r: GroupRules): Rules => ({
  ...r,
  excludeGroups: r.excludeGroups ?? [],
  groupOrder: r.groupOrder ?? [],
});

/**
 * The provider's channel groups: which are imported, whether new ones join
 * automatically, adult groups, and the channel numbering order. Edits stay in
 * the draft; the source re-imports once, on save.
 */
export function ChannelGroups({
  groups,
  rules,
  onChange,
}: {
  groups: SourceGroup[];
  rules: GroupRules;
  /** Receives an updater so memoised rows never act on stale rules. */
  onChange: (update: (rules: Rules) => Partial<GroupRules>) => void;
}) {
  const [q, setQ] = useState("");
  const [orderMode, setOrderMode] = useState<"provider" | "mine">(
    rules.groupOrder?.length ? "mine" : "provider",
  );
  const hideAdult = Boolean(rules.hideAdult);
  const excluded = useMemo(
    () => new Set((rules.excludeGroups ?? []).map(normGroup)),
    [rules.excludeGroups],
  );
  const isOut = useCallback(
    (g: SourceGroup) => excluded.has(normGroup(g.name)) || (hideAdult && Boolean(g.adult)),
    [excluded, hideAdult],
  );
  const included = useMemo(() => groups.filter((g) => !isOut(g)), [groups, isOut]);
  const channels = useMemo(() => included.reduce((n, g) => n + g.channels, 0), [included]);
  const needle = q.trim().toLowerCase();
  const shown = useMemo(
    () => (needle ? groups.filter((g) => g.name.toLowerCase().includes(needle)) : groups),
    [groups, needle],
  );

  const update = onChange;
  const toggle = useCallback(
    (g: SourceGroup, include: boolean) =>
      update((r) => {
        const key = normGroup(g.name);
        const rest = read(r).excludeGroups.filter((name) => normGroup(name) !== key);
        return { excludeGroups: include ? rest : [...rest, g.name] };
      }),
    [update],
  );
  // With a search, the quick actions apply to the matching groups only.
  const setAll = (include: boolean) =>
    update((r) => {
      const keys = new Set(shown.map((g) => normGroup(g.name)));
      const rest = read(r).excludeGroups.filter((name) => !keys.has(normGroup(name)));
      return { excludeGroups: include ? rest : [...rest, ...shown.map((g) => g.name)] };
    });

  // Included groups in the owner's order; groups not yet ordered follow in
  // the provider's order, exactly as the server numbers them.
  const ordered = useMemo(() => {
    const byKey = new Map(included.map((g) => [normGroup(g.name), g]));
    const out: SourceGroup[] = [];
    for (const name of rules.groupOrder ?? []) {
      const g = byKey.get(normGroup(name));
      if (g) {
        out.push(g);
        byKey.delete(normGroup(name));
      }
    }
    return [...out, ...byKey.values()];
  }, [included, rules.groupOrder]);
  const reorder = (next: SourceGroup[]) => update(() => ({ groupOrder: next.map((g) => g.name) }));
  const move = (from: number, to: number) => {
    if (to < 0 || to >= ordered.length || from === to) return;
    const next = [...ordered];
    const [g] = next.splice(from, 1);
    next.splice(to, 0, g!);
    reorder(next);
  };

  return (
    <div className="space-y-5">
      <div
        aria-live="polite"
        className={cn(
          "rounded-xl border px-4 py-3",
          channels === 0 ? "border-warning/40 bg-warning/10" : "border-border bg-bg-subtle/60",
        )}
      >
        <p className="text-sm font-semibold text-fg">
          {channels === 0
            ? t("Nothing will be imported. Include at least one group.")
            : tn(channels, "{count} channel will be imported", "{count} channels will be imported")}
        </p>
        <p className="text-xs text-fg-muted">
          {tn(
            included.length,
            "{count} of {total} group included",
            "{count} of {total} groups included",
            { total: formatNumber(groups.length) },
          )}
        </p>
      </div>

      <div className="space-y-2">
        <div className="flex flex-wrap items-center gap-2">
          <div className="relative min-w-0 flex-1">
            <Search
              size={13}
              className="pointer-events-none absolute left-2.5 top-1/2 -translate-y-1/2 text-fg-subtle"
              aria-hidden
            />
            <Input
              type="search"
              value={q}
              onChange={(e) => setQ(e.currentTarget.value)}
              placeholder={t("Find a group…")}
              aria-label={t("Find a group")}
              className="h-8 pl-7 text-sm"
            />
          </div>
          <Button variant="outline" size="sm" disabled={!shown.length} onClick={() => setAll(true)}>
            {needle ? t("Include matches") : t("Include all")}
          </Button>
          <Button
            variant="outline"
            size="sm"
            disabled={!shown.length}
            onClick={() => setAll(false)}
          >
            {needle ? t("Leave matches out") : t("Leave all out")}
          </Button>
        </div>
        <ul
          aria-label={t("Channel groups")}
          className="app-scroll max-h-[22rem] divide-y divide-border/60 overflow-y-auto rounded-xl border border-border bg-surface"
        >
          {shown.map((g) => (
            <GroupRow
              key={g.name}
              group={g}
              included={!isOut(g)}
              adultHidden={hideAdult && Boolean(g.adult)}
              onToggle={toggle}
            />
          ))}
          {shown.length === 0 ? (
            <li className="px-4 py-6 text-center text-sm text-fg-muted">{t("No groups match.")}</li>
          ) : null}
        </ul>
      </div>

      <div className="space-y-2">
        <SwitchRow
          label={t("Include new groups automatically")}
          hint={
            rules.newGroups === "exclude"
              ? t("Groups your provider adds later stay out until you include them here.")
              : t("Groups your provider adds later are imported on the next refresh.")
          }
          checked={rules.newGroups !== "exclude"}
          onChange={(on) => update(() => ({ newGroups: on ? "" : "exclude" }))}
        />
        <SwitchRow
          label={t("Hide adult channels")}
          hint={t("Leaves out every group your provider marks as adult, now and later.")}
          checked={hideAdult}
          onChange={(on) => update(() => ({ hideAdult: on }))}
        />
      </div>

      <div className="space-y-3">
        <div className="space-y-1.5">
          <p className="text-sm font-medium text-fg">{t("Channel numbers")}</p>
          <Segmented
            ariaLabel={t("Channel numbers")}
            size="sm"
            value={orderMode}
            onChange={(mode) => {
              setOrderMode(mode);
              update(() => ({
                groupOrder: mode === "mine" ? included.map((g) => g.name) : [],
              }));
            }}
            options={[
              { value: "provider", label: t("Provider's order (recommended)") },
              { value: "mine", label: t("My order") },
            ]}
          />
          <p className="text-xs text-fg-muted">
            {orderMode === "mine"
              ? t(
                  "Channels are numbered 1, 2, 3… down your group order. Drag a group or use its arrows.",
                )
              : t(
                  "Channels keep your provider's numbers and order, which usually puts live events first.",
                )}
          </p>
        </div>
        {orderMode === "mine" && ordered.length ? (
          <SortableList
            items={ordered}
            getId={(g) => g.name}
            onReorder={reorder}
            className="app-scroll max-h-[22rem] divide-y divide-border/60 overflow-y-auto rounded-xl border border-border bg-surface"
            renderItem={(g, handle, i) => (
              <div className="flex items-center gap-2 bg-surface px-2 py-1">
                <DragHandle handle={handle} label={t("Move {group}", { group: groupName(g) })} />
                <span className="w-8 shrink-0 text-right font-mono text-xs tabular-nums text-fg-subtle">
                  {formatNumber(i + 1)}
                </span>
                <span className="min-w-0 flex-1 truncate text-sm text-fg">{groupName(g)}</span>
                <span className="shrink-0 text-xs tabular-nums text-fg-subtle">
                  {formatNumber(g.channels)}
                </span>
                <OrderButton
                  label={t("Move {group} to the top", { group: groupName(g) })}
                  disabled={i === 0}
                  onClick={() => move(i, 0)}
                >
                  <ArrowUpToLine size={14} />
                </OrderButton>
                <OrderButton
                  label={t("Move {group} up", { group: groupName(g) })}
                  disabled={i === 0}
                  onClick={() => move(i, i - 1)}
                >
                  <ArrowUp size={14} />
                </OrderButton>
                <OrderButton
                  label={t("Move {group} down", { group: groupName(g) })}
                  disabled={i === ordered.length - 1}
                  onClick={() => move(i, i + 1)}
                >
                  <ArrowDown size={14} />
                </OrderButton>
              </div>
            )}
          />
        ) : null}
      </div>
    </div>
  );
}

const groupName = (g: SourceGroup) => g.name.trim() || t("No group");

const GroupRow = memo(function GroupRow({
  group: g,
  included,
  adultHidden,
  onToggle,
}: {
  group: SourceGroup;
  included: boolean;
  adultHidden: boolean;
  onToggle: (g: SourceGroup, include: boolean) => void;
}) {
  return (
    <li>
      <label
        className={cn(
          "flex items-center gap-3 px-3 py-2 text-sm",
          adultHidden ? "cursor-not-allowed opacity-60" : "cursor-pointer hover:bg-surface-hover",
        )}
      >
        <Checkbox
          checked={included}
          disabled={adultHidden}
          onChange={(e) => onToggle(g, e.currentTarget.checked)}
        />
        <span className={cn("min-w-0 flex-1 truncate", included ? "text-fg" : "text-fg-muted")}>
          {groupName(g)}
        </span>
        {g.adult ? (
          <Badge variant={adultHidden ? "secondary" : "warning"}>
            {adultHidden ? t("Adult · hidden") : t("Adult")}
          </Badge>
        ) : null}
        <span className="shrink-0 text-xs tabular-nums text-fg-subtle">
          {tn(g.channels, "{count} channel", "{count} channels")}
        </span>
      </label>
    </li>
  );
});

function SwitchRow({
  label,
  hint,
  checked,
  onChange,
}: {
  label: string;
  hint: string;
  checked: boolean;
  onChange: (on: boolean) => void;
}) {
  return (
    <label className="flex cursor-pointer items-center gap-3 rounded-xl border border-border bg-surface p-3">
      <span className="min-w-0 flex-1">
        <span className="block text-sm font-medium text-fg">{label}</span>
        <span className="block text-xs text-fg-muted">{hint}</span>
      </span>
      <Switch
        aria-label={label}
        checked={checked}
        onChange={(e) => onChange(e.currentTarget.checked)}
      />
    </label>
  );
}

function OrderButton({
  label,
  disabled,
  onClick,
  children,
}: {
  label: string;
  disabled: boolean;
  onClick: () => void;
  children: React.ReactNode;
}) {
  return (
    <Button
      variant="ghost"
      size="sm"
      className="size-8 shrink-0 p-0 text-fg-subtle hover:text-fg"
      aria-label={label}
      title={label}
      disabled={disabled}
      onClick={onClick}
    >
      {children}
    </Button>
  );
}
