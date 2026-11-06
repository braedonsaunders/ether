import { t } from "../../../../i18n";
import type { Style, StyleScene } from "../studio-model";
export function sceneDisplayName(style: Style, scene: StyleScene): string {
  const generic =
    !scene.name.trim() ||
    scene.name === style.name ||
    scene.name.toLowerCase().replace(/[\s_-]/g, "") === scene.role;
  if (!generic) return scene.name;
  const names = {
    ident: t("{template} ident", { template: style.name }),
    upnext: t("Up next card"),
    tonight: t("Tonight lineup"),
    break: t("Break bumper"),
    back: t("Back from break"),
    offair: t("Off air card"),
    welcome: t("Welcome card"),
    lowerthird: t("Programme lower third"),
    bug: t("Corner logo"),
  };
  const peers = style.scenes.filter((s) => s.role === scene.role);
  return peers.length > 1
    ? t("{name} {number}", {
        name: names[scene.role],
        number: peers.findIndex((s) => s.id === scene.id) + 1,
      })
    : names[scene.role];
}
export function nameTemplateScenes(style: Style): Style {
  return {
    ...style,
    scenes: style.scenes.map((scene) => ({ ...scene, name: sceneDisplayName(style, scene) })),
  };
}
