import { styleCopy } from "./naming";
import { Button, Input, Select, Switch } from "@braedonsaunders/appkit-ui";
import { useState } from "react";
import { useSearchParams } from "react-router-dom";
import { MoreOptions } from "../../../components/ConfigSection";
import { SettingsRow } from "../../../components/SettingsRow";
import { ToolbarPicker } from "../../../components/ToolbarPicker";
import { Field } from "../../../components/fields";
import { ErrorPanel } from "../../../components/feedback";
import { useAsync } from "../../../lib/hooks";
import { api } from "../../../lib/api";
import { t } from "../../../i18n";
import type { Channel } from "../model";
import { onair, type ChannelLook, type Moment } from "./model";
import { LookTile } from "./Stage";
import { InfoTip } from "./RecipeTab";
import { ChannelScreens } from "./ScreenPicker";

import { BugControls, resolveBug } from "./BugControls";
export function LookTab({
  channel,
  onChange,
  viewerName,
}: {
  channel: Channel;
  onChange: (patch: Partial<Channel>) => void;
  viewerName?: string;
}) {
  const config = useAsync(() => api.getConfig(), []);
  const looks = useAsync(() => onair.looks(), []);
  const bindings = useAsync(
    () =>
      onair.bindings({
        channel: channel.id,
        at: new Date(new Date().setHours(20, 0, 0, 0)).toISOString(),
      }),
    [channel.id],
  );
  const [, setParams] = useSearchParams();
  const [moment, setMoment] = useState<Moment>("ident");
  const settings: ChannelLook = channel.look ?? {
    lookId: "",
    ident: true,
    upNext: true,
    offAir: true,
    breaks: "",
    breakContent: "look",
    breakSeconds: 60,
  };
  const set = (patch: Partial<ChannelLook>) => onChange({ look: { ...settings, ...patch } });
  const baseLook = looks.data?.find(
    (v) => v.id === (settings.lookId || config.data?.liveTV?.defaultStyleId),
  );
  const look = baseLook
    ? {
        ...baseLook,
        brand: {
          ...baseLook.brand,
          tagline: settings.tagline || baseLook.brand.tagline,
          palette: {
            ...baseLook.brand.palette,
            accent: settings.accent || baseLook.brand.palette.accent,
          },
        },
      }
    : undefined;
  const navigateLook = (id: string) =>
    setParams((p) => {
      p.set("look", id);
      return p;
    });
  if (channel.kind === "iptv") return <ChannelScreens channel={channel} onChange={onChange} />;
  return (
    <div className="space-y-6">
      <h3 className="text-sm font-semibold">
        {settings.lookId
          ? t("Override for {name}", { name: channel.name })
          : styleCopy("Default style · {name}", { name: look?.name ?? styleCopy("No style") })}
      </h3>
      {looks.error ? <ErrorPanel error={looks.error} onRetry={() => void looks.reload()} /> : null}
      <div className="grid min-w-0 grid-cols-1 gap-4 sm:grid-cols-[minmax(0,1fr)_12rem]">
        {look ? (
          <LookTile
            look={{ ...look, bug: resolveBug(look.bug, settings.bug) }}
            channel={
              channel.personal
                ? channel.personal.nameFormat.replace("{viewer}", viewerName || "")
                : channel.name
            }
            moment={settings.bug ? "bug" : moment}
            bindings={{
              ...bindings.data,
              channel: { name: channel.name, logo: channel.logo },
              viewer: { name: viewerName || "" },
            }}
          />
        ) : (
          <div className="grid aspect-video place-items-center rounded-lg border border-dashed border-border bg-surface text-sm text-fg-muted">
            {styleCopy("Choose a style")}
          </div>
        )}
        <div className="flex min-w-0 flex-col items-start gap-3">
          <ToolbarPicker
            ariaLabel={styleCopy("Style")}
            value={settings.lookId}
            onChange={(id) => (id === "new" ? navigateLook("new") : set({ lookId: id }))}
            className="w-full"
            menuClassName="max-w-[calc(100vw-2rem)]"
            trigger={
              <>
                {look ? (
                  <LookTile look={look} size="h-5 w-9 shrink-0 rounded-sm" playOnHover={false} />
                ) : null}
                <span className="min-w-0 flex-1 truncate text-left">{look?.name || t("None")}</span>
              </>
            }
            options={[
              { value: "", label: styleCopy("Use default style") },
              ...(looks.data ?? []).map((v) => ({
                value: v.id,
                label: v.name,
                icon: (
                  <LookTile
                    look={v}
                    size="h-7 w-12 rounded-sm"
                    bindings={{ ...bindings.data, viewer: { name: viewerName || "" } }}
                    playOnHover={false}
                  />
                ),
              })),
              { value: "new", label: styleCopy("New style…") },
            ]}
          />
          {look ? (
            <Button variant="outline" onClick={() => navigateLook(look.id)}>
              {styleCopy("Edit style")}
            </Button>
          ) : null}
        </div>
      </div>
      {look ? (
        <div className="grid grid-cols-3 gap-2 sm:grid-cols-6" aria-label={t("Moments")}>
          {(["ident", "tonight", "upnext", "break", "back", "offair"] as Moment[]).map((m, i) => (
            <button
              type="button"
              key={m}
              aria-pressed={m === moment}
              onClick={() => setMoment(m)}
              className={`rounded-lg border p-1 text-xs ${m === moment ? "border-primary ring-1 ring-primary" : "border-border"}`}
            >
              <LookTile
                look={look}
                moment={m}
                bindings={{ ...bindings.data, viewer: { name: viewerName || "" } }}
                playOnHover={false}
              />
              <span className="block py-1">
                {[t("Ident"), t("Tonight"), t("Up next"), t("Break"), t("Back"), t("Off air")][i]}
              </span>
            </button>
          ))}
        </div>
      ) : null}
      <section className="divide-y divide-border">
        <h3 className="pb-2 text-xs font-semibold uppercase tracking-wider text-fg-muted">
          {t("On this channel")}
        </h3>
        <SettingsRow
          title={t("Off-air screen")}
          control={
            <Switch
              checked={settings.offAir}
              aria-label={t("Off-air screen")}
              onChange={(e) => set({ offAir: e.currentTarget.checked })}
            />
          }
        />
        <SettingsRow
          title={
            <span className="flex items-center gap-2">
              {t("Corner logo and lower thirds")}
              <InfoTip
                text={t(
                  "Needs transcoding. Ether only adds it when the stream is already being converted, unless you choose Always.",
                )}
              />
            </span>
          }
          control={
            <Select
              aria-label={t("Corner logo and lower thirds")}
              value={settings.overlays || ""}
              onChange={(e) => set({ overlays: e.currentTarget.value as ChannelLook["overlays"] })}
            >
              <option value="">{t("Off")}</option>
              <option value="whenTranscoding">{t("When transcoding")}</option>
              <option value="always">{t("Always")}</option>
            </Select>
          }
        />
      </section>
      {look ? (
        <section className="space-y-4 border-t border-border pt-4">
          <label className="flex items-center justify-between gap-3 text-sm font-medium">
            {t("Customize corner logo")}
            <Switch
              aria-label={t("Customize corner logo")}
              checked={settings.bug !== undefined}
              onChange={(e) => {
                set({ bug: e.currentTarget.checked ? resolveBug(look.bug) : undefined });
                setMoment("bug");
              }}
            />
          </label>
          {settings.bug ? (
            <BugControls
              value={resolveBug(look.bug, settings.bug)}
              onChange={(bug) => set({ bug })}
            />
          ) : (
            <p className="text-sm text-fg-muted">{t("Uses the look’s corner logo.")}</p>
          )}
        </section>
      ) : null}
      <MoreOptions label={t("More options")}>
        <div className="grid gap-4 p-4 sm:grid-cols-3">
          <Field label={t("Tagline")}>
            <Input
              value={settings.tagline || ""}
              onChange={(e) => set({ tagline: e.currentTarget.value })}
            />
          </Field>
          <Field label={t("Accent")}>
            <Input
              type="color"
              value={settings.accent || look?.brand.palette.accent || "#00b8a9"}
              onChange={(e) => set({ accent: e.currentTarget.value })}
            />
          </Field>
        </div>
      </MoreOptions>
      <ChannelScreens channel={channel} onChange={onChange} />
    </div>
  );
}
