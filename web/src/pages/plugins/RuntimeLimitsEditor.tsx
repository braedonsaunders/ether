import { useEffect, useState } from "react";
import { Button, Input } from "@braedonsaunders/appkit-ui";
import { apiFetch } from "../../lib/api";
import { t } from "../../i18n";
import type { RuntimeLimits } from "./types";

type Policy = {
  runtimeLimits?: Partial<RuntimeLimits>;
  runtimeOverrides?: Record<string, Partial<RuntimeLimits>>;
};
function hours(value?: string) {
  if (!value) return "0";
  const matches = [...value.matchAll(/([\d.]+)(h|m|s)/g)];
  return String(
    matches.reduce(
      (sum, m) => sum + Number(m[1]) * (m[2] === "h" ? 1 : m[2] === "m" ? 1 / 60 : 1 / 3600),
      0,
    ),
  );
}
export function RuntimeLimitsEditor({
  plugin = "",
  onSaved,
}: {
  plugin?: string;
  onSaved?: () => void;
}) {
  const [memory, setMemory] = useState("0");
  const [time, setTime] = useState("0");
  const [units, setUnits] = useState("0");
  const [fetchSize, setFetchSize] = useState("0");
  const [memberSize, setMemberSize] = useState("0");
  const [totalSize, setTotalSize] = useState("0");
  const [members, setMembers] = useState("0");
  const [ready, setReady] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [saved, setSaved] = useState(false);
  useEffect(() => {
    let live = true;
    void apiFetch<Policy>("/plugins/runtime-limits")
      .then((policy) => {
        if (!live) return;
        const limits = (plugin ? policy.runtimeOverrides?.[plugin] : policy.runtimeLimits) ?? {};
        setMemory(String((limits.memoryBytes ?? 0) / 1048576));
        setTime(hours(limits.deadline));
        setUnits(String(limits.unitBudget ?? 0));
        setFetchSize(String((limits.fetchBytes ?? 0) / 1073741824));
        setMemberSize(String((limits.archiveMemberBytes ?? 0) / 1073741824));
        setTotalSize(String((limits.archiveExpandedBytes ?? 0) / 1073741824));
        setMembers(String(limits.archiveMembers ?? 0));
        setReady(true);
      })
      .catch((e) => {
        if (live) setError(String(e));
      });
    return () => {
      live = false;
    };
  }, [plugin]);
  return (
    <div className="space-y-3 rounded-xl border border-border bg-surface p-4">
      <h3 className="text-sm font-medium">{t("Limits")}</h3>
      <p className="text-xs text-fg-muted">
        {t(
          "Zero inherits the default. Global defaults are 2048 MiB, 12 hours, and unlimited work units. Saving stops current jobs.",
        )}
      </p>
      <div className="grid gap-3 sm:grid-cols-3">
        <label className="space-y-1 text-xs">
          {t("Memory (MiB)")}
          <Input
            type="number"
            min={0}
            max={4096}
            value={memory}
            onChange={(e) => setMemory(e.target.value)}
          />
        </label>
        <label className="space-y-1 text-xs">
          {t("Time (hours)")}
          <Input
            type="number"
            min={0}
            max={8760}
            step="any"
            value={time}
            onChange={(e) => setTime(e.target.value)}
          />
        </label>
        <label className="space-y-1 text-xs">
          {t("Work units")}
          <Input
            type="number"
            min={0}
            step={1}
            value={units}
            onChange={(e) => setUnits(e.target.value)}
          />
        </label>
        <label className="space-y-1 text-xs">
          {t("Download (GiB)")}
          <Input
            type="number"
            min={0}
            step="any"
            value={fetchSize}
            onChange={(e) => setFetchSize(e.target.value)}
          />
        </label>
        <label className="space-y-1 text-xs">
          {t("Archive member (GiB)")}
          <Input
            type="number"
            min={0}
            step="any"
            value={memberSize}
            onChange={(e) => setMemberSize(e.target.value)}
          />
        </label>
        <label className="space-y-1 text-xs">
          {t("Archive total (GiB)")}
          <Input
            type="number"
            min={0}
            step="any"
            value={totalSize}
            onChange={(e) => setTotalSize(e.target.value)}
          />
        </label>
        <label className="space-y-1 text-xs">
          {t("Archive members")}
          <Input
            type="number"
            min={0}
            max={1000000}
            value={members}
            onChange={(e) => setMembers(e.target.value)}
          />
        </label>
      </div>
      {error ? (
        <p role="alert" className="text-xs text-danger">
          {error}
        </p>
      ) : null}
      <Button
        size="sm"
        disabled={!ready || busy}
        onClick={() => {
          setBusy(true);
          setError("");
          setSaved(false);
          const limits = {
            memoryBytes: Math.round(Number(memory) * 1048576),
            deadline: Number(time) > 0 ? `${Number(time)}h` : "",
            unitBudget: Number(units),
            fetchBytes: Math.round(Number(fetchSize) * 1073741824),
            archiveMemberBytes: Math.round(Number(memberSize) * 1073741824),
            archiveExpandedBytes: Math.round(Number(totalSize) * 1073741824),
            archiveMembers: Number(members),
          };
          void apiFetch("/plugins/runtime-limits", {
            method: "PUT",
            body: JSON.stringify({ plugin, limits }),
          })
            .then(() => {
              setSaved(true);
              onSaved?.();
            })
            .catch((e) => setError(String(e)))
            .finally(() => setBusy(false));
        }}
      >
        {saved ? t("Saved") : busy ? t("Saving…") : t("Save")}
      </Button>
    </div>
  );
}
