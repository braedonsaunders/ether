import { Select } from "@braedonsaunders/appkit-ui";
import { Field } from "../../components/fields";
import { t } from "../../i18n";

/** "Strict" or "Prefer, with fallback", as the save dialogs and lists name a pin's mode. */
export function pinModeLabel(fallback: boolean | undefined) {
  return fallback ? t("Prefer, with fallback") : t("Strict");
}

/** How a pin behaves when its release can't play. Strict is the default. */
export function PinModeField({
  id,
  fallback,
  onChange,
}: {
  id: string;
  fallback: boolean;
  onChange: (fallback: boolean) => void;
}) {
  return (
    <Field
      label={t("Mode")}
      htmlFor={id}
      hint={
        fallback
          ? t(
              "Auto tries this release first for every viewer, whatever their quality profile. If it can't play, Ether picks the next best release as usual and tries this one again later. A viewer can still pick another version, and offline downloads keep their own profile.",
            )
          : t(
              "Auto plays only this release for every viewer, whatever their quality profile. If it fails its health check, Auto playback stops and the pin shows why. A viewer can still pick another version, and offline downloads keep their own profile.",
            )
      }
    >
      <Select
        id={id}
        aria-label={t("Mode")}
        value={fallback ? "fallback" : "strict"}
        onChange={(e) => onChange(e.target.value === "fallback")}
      >
        <option value="strict">{pinModeLabel(false)}</option>
        <option value="fallback">{pinModeLabel(true)}</option>
      </Select>
    </Field>
  );
}
