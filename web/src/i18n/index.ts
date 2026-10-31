import {
  createSystemTranslator,
  DEFAULT_LOCALE,
  isAppLocale,
  LOCALE_OPTIONS,
  negotiateLocale,
  type AppLocale,
  type MessageCatalog,
  type MessageValues,
} from "@braedonsaunders/appkit-i18n";
import { createElement, Fragment, type ReactNode } from "react";

/**
 * Ether's copy runs on AppKit's exact-copy translator: the English string in
 * the code is the message, and `locales/<code>.json` holds each language's
 * translations keyed by `systemMessageKey(english)`. `pnpm i18n` extracts
 * every literal passed to `t`, `tn`, `tx` and `msg` and manages catalogues.
 *
 * Call these while rendering or handling an event, never at module scope: a
 * module-level `t()` runs once, before the owner picks a language. Build
 * module-level tables with `localized(() => …)`, or mark their strings with
 * `msg()` and translate where they are shown, `t(LABELS[key])`.
 */

export type Locale = AppLocale;
export type Values = MessageValues;

/** Every shipped language; English first, the rest by their own names. */
export const LOCALES = [
  LOCALE_OPTIONS[0],
  ...LOCALE_OPTIONS.slice(1).sort((a, b) => a.nativeLabel.localeCompare(b.nativeLabel, "en")),
];

const STORAGE_KEY = "ether.locale";

const loaders = import.meta.glob<{ default: MessageCatalog }>([
  "./locales/*.json",
  "!./locales/en.json",
]);

/** Fetches one locale's catalogue; tests substitute a fake. */
export type CatalogLoader = (locale: Locale) => Promise<MessageCatalog>;

const loadCatalog: CatalogLoader = async (locale) => {
  const load = loaders[`./locales/${locale}.json`];
  if (!load) throw new Error(`no catalogue for ${locale}`);
  return (await load()).default;
};

let translator = createSystemTranslator(DEFAULT_LOCALE);
let generation = 0;
let latestRequest = 0;

/** The language currently rendered. */
export function getLocale(): Locale {
  return translator.locale;
}

/** The owner's saved console language, when they have chosen one. */
export function rememberedLocale(): Locale | null {
  try {
    const saved = localStorage.getItem(STORAGE_KEY);
    return isAppLocale(saved) ? saved : null;
  } catch {
    // Storage can be unavailable (privacy modes); nothing was saved.
    return null;
  }
}

/** The owner's saved choice, else the browser's best supported match. */
export function preferredLocale(): Locale {
  const saved = rememberedLocale();
  if (saved) return saved;
  if (typeof navigator === "undefined") return DEFAULT_LOCALE;
  return negotiateLocale(navigator.languages?.length ? navigator.languages : [navigator.language]);
}

export function rememberLocale(locale: Locale): void {
  try {
    localStorage.setItem(STORAGE_KEY, locale);
  } catch {
    // Not persisting is acceptable; the choice still applies to this tab.
  }
}

/**
 * Load a locale's catalogue and make it active. The latest request wins: an
 * earlier load that finishes later resolves `false` without applying. A
 * failed load rejects and leaves the active language unchanged.
 */
export async function loadLocale(locale: Locale, load = loadCatalog): Promise<boolean> {
  const request = ++latestRequest;
  const catalog = locale === DEFAULT_LOCALE ? {} : await load(locale);
  if (request !== latestRequest) return false;
  translator = createSystemTranslator(locale, catalog);
  generation++;
  if (typeof document !== "undefined") document.documentElement.lang = locale;
  return true;
}

/**
 * First load at startup: the preferred language, or the bundled English when
 * its catalogue cannot be fetched. The saved preference is kept, so the next
 * visit tries it again.
 */
export async function startLocale(load = loadCatalog): Promise<Locale> {
  try {
    await loadLocale(preferredLocale(), load);
  } catch (err) {
    console.warn("Ether: falling back to English; the saved language failed to load", err);
    await loadLocale(DEFAULT_LOCALE, load);
  }
  return getLocale();
}

/** Marks a module-level string for extraction without translating it yet. */
export function msg<T extends string>(source: T): T {
  return source;
}

/** Translate `source`, filling `{name}` placeholders from `values`. */
export function t(source: string, values?: Values): string {
  return translator(source, values);
}

/**
 * Translate a count-dependent phrase: `tn(n, "{count} title", "{count} titles")`.
 * `{count}` is filled with the locale-formatted number.
 */
export function tn(count: number, one: string, other: string, values?: Values): string {
  return translator.plural(count, one, other, values);
}

/**
 * Translate a phrase with React elements in it:
 * `tx("Read the {guide} first", { guide: <a href=…>{t("setup guide")}</a> })`.
 */
export function tx(source: string, values: Readonly<Record<string, ReactNode>>): ReactNode {
  const children = translator(source)
    .split(/(\{\w+\})/)
    .map((part) => {
      const name = /^\{(\w+)\}$/.exec(part)?.[1];
      if (name === undefined || !(name in values)) return part;
      const value = values[name];
      return Array.isArray(value) ? createElement(Fragment, null, ...value) : value;
    });
  return createElement(Fragment, null, ...children);
}

/**
 * A module-level object or array whose copy follows the active language:
 * `export const LABELS = localized(() => ({ ok: t("Healthy") }))`. It reads
 * exactly like the plain value; `build` re-runs after each language switch.
 */
export function localized<T extends object>(build: () => T): T {
  let value = build();
  let built = generation;
  const current = (): T => {
    if (built !== generation) {
      value = build();
      built = generation;
    }
    return value;
  };
  return new Proxy(value, {
    get: (_, key) => Reflect.get(current(), key),
    has: (_, key) => Reflect.has(current(), key),
    ownKeys: () => Reflect.ownKeys(current()),
    getOwnPropertyDescriptor: (_, key) => Reflect.getOwnPropertyDescriptor(current(), key),
  });
}

/**
 * Search aliases in the active language plus the English, so command search
 * still finds pages by the English terms (and brand names) owners know.
 */
export function searchTerms(source: string): string {
  const translated = translator(source);
  return translated === source ? source : `${translated} ${source}`;
}
