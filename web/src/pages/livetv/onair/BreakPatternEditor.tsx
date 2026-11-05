import { Input, Select, Switch } from "@braedonsaunders/appkit-ui";
import { SettingsRow } from "../../../components/SettingsRow";
import { t } from "../../../i18n";
import type { BreakPattern } from "./model";

export const breakPatternPreset = (era: string): BreakPattern => ({
  preset: era,
  seconds: era === "1980s" ? 120 : era === "1990s" ? 150 : era === "2000s" ? 180 : 210,
  minSeconds: 60,
  maxSeconds: era === "2000s" ? 240 : era === "2010s" ? 270 : 180,
  spots: era === "1980s" ? 4 : era === "1990s" ? 5 : era === "2000s" ? 6 : 7,
  minSpots: 2,
  maxSpots: era === "2000s" ? 8 : era === "2010s" ? 9 : 6,
  podsPerHalfHour: era === "1980s" || era === "1990s" ? 2 : 3,
  inside: true,
  noRepeatMinutes: 60,
  stationId: era === "1980s" || era === "1990s",
  bumperStyle: "break-back",
  clockAlignment: era === "1980s" ? "half" : "none",
});
export function BreakPatternEditor({
  value,
  onChange,
}: {
  value?: BreakPattern;
  onChange: (p?: BreakPattern) => void;
}) {
  const update = (patch: Partial<BreakPattern>) =>
    onChange({ ...value!, ...patch, preset: "custom" });
  return (
    <div className="space-y-3">
      <SettingsRow
        title={t("Break pattern")}
        control={
          <Select
            aria-label={t("Break pattern")}
            value={value?.preset || ""}
            onChange={(e) =>
              onChange(
                e.currentTarget.value === "custom"
                  ? { ...(value || breakPatternPreset("1990s")), preset: "custom" }
                  : e.currentTarget.value
                    ? breakPatternPreset(e.currentTarget.value)
                    : undefined,
              )
            }
          >
            <option value="">{t("Fixed length")}</option>
            <option value="1980s">{t("1980s")}</option>
            <option value="1990s">{t("1990s")}</option>
            <option value="2000s">{t("2000s")}</option>
            <option value="2010s">{t("2010s")}</option>
            <option value="custom">{t("Custom")}</option>
          </Select>
        }
      />
      {value ? (
        <>
          <div className="grid grid-cols-2 gap-3">
            {(
              [
                ["seconds", t("Pod length (seconds)"), 0, 600],
                ["spots", t("Spots per pod"), 0, 30],
                ["minSeconds", t("Minimum seconds"), 0, 600],
                ["maxSeconds", t("Maximum seconds"), 1, 600],
                ["minSpots", t("Minimum spots"), 0, 30],
                ["maxSpots", t("Maximum spots"), 1, 30],
                ["podsPerHalfHour", t("Pods per half hour"), 1, 6],
                ["noRepeatMinutes", t("No repeats (minutes)"), 0, 1440],
              ] as const
            ).map(([key, label, min, max]) => (
              <label key={key} className="space-y-1 text-sm text-fg-muted">
                <span>{label}</span>
                <Input
                  className="w-full"
                  type="number"
                  min={min}
                  max={max}
                  value={value[key]}
                  onChange={(e) => update({ [key]: Number(e.currentTarget.value) })}
                />
              </label>
            ))}
          </div>
          <SettingsRow
            title={t("Station ID")}
            control={
              <Switch
                aria-label={t("Station ID")}
                checked={value.stationId}
                onChange={(e) => update({ stationId: e.currentTarget.checked })}
              />
            }
          />
        </>
      ) : null}
    </div>
  );
}
