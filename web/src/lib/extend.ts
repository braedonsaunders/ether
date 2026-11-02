/**
 * The descriptor registry: the one place the console learns what exists.
 * Mirrors internal/extend (docs/extend/SPEC.md §5.9). Built-ins and plugins
 * use the same shape; pickers, cards, navigation and commands render from it
 * and never keep their own copy of the list.
 */
import { useEffect, useMemo, useState } from "react";
import { apiFetch } from "./api";
import { t } from "../i18n";
import { useLocale } from "../i18n/react";

export type DescriptorKind =
  | "rankingStep"
  | "ruleField"
  | "collectionSource"
  | "rowSource"
  | "liveTVSource"
  | "channelKind"
  | "eventKind"
  | "subtitleProvider"
  | "markerSource"
  | "sourceKind"
  | "integration"
  | "page"
  | "navItem"
  | "command"
  | "settingsSchema"
  | "slot"
  | "widget"
  | "tool";

/** User-visible copy: an English source string (built-ins) or a plugin string. */
export interface ExtendText {
  source?: string;
  plugin?: string;
  key?: string;
  /** Plugin strings arrive already resolved for the requested locale. */
  resolved?: string;
}

export interface Descriptor<M = unknown> {
  kind: DescriptorKind;
  id: string;
  /** "builtin" or a plugin id. */
  owner: string;
  label: ExtendText;
  hint?: ExtendText;
  /** lucide icon name (built-ins) or an asset URL (plugins). */
  icon?: string;
  group?: string;
  order?: number;
  meta?: M;
}

export const BUILTIN = "builtin";

export function isPluginDescriptor(d: Descriptor): boolean {
  return d.owner !== BUILTIN;
}

/** Copy for a descriptor field in the active language. */
export function textOf(text?: ExtendText): string {
  if (!text) return "";
  if (text.resolved) return text.resolved;
  // Built-in copy is declared in Go with extend.T and extracted into the
  // console catalogue, so translating a server-sent source string is safe.
  if (text.source) return t(text.source);
  return text.key ?? "";
}

interface RegistryResponse {
  version: number;
  descriptors: Descriptor[];
}

type Entry = { version: number; descriptors: Descriptor[]; at: number };
const cache = new Map<string, Entry>();
const inflight = new Map<string, Promise<Entry>>();
const listeners = new Set<() => void>();

function cacheKey(kinds: readonly DescriptorKind[], locale: string): string {
  return [...kinds].sort().join(",") + "|" + locale;
}

async function load(kinds: readonly DescriptorKind[], locale: string): Promise<Entry> {
  const key = cacheKey(kinds, locale);
  const pending = inflight.get(key);
  if (pending) return pending;
  const query = new URLSearchParams();
  if (kinds.length) query.set("kinds", [...kinds].sort().join(","));
  query.set("locale", locale);
  const p = apiFetch<RegistryResponse>(`/extend/registry?${query}`)
    .then((r) => {
      const entry = { version: r.version, descriptors: r.descriptors ?? [], at: Date.now() };
      cache.set(key, entry);
      return entry;
    })
    .finally(() => inflight.delete(key));
  inflight.set(key, p);
  return p;
}

/**
 * Drop cached descriptors and re-fetch everywhere. The console stream calls
 * this on extend.changed; plugin installs call it after they finish.
 */
export function invalidateRegistry(): void {
  cache.clear();
  for (const fn of listeners) fn();
}

export interface RegistryState<M = unknown> {
  descriptors: Descriptor<M>[];
  loading: boolean;
  error: Error | null;
  reload: () => void;
}

/** Descriptors of the given kinds, in registry order. */
export function useRegistry<M = unknown>(...kinds: DescriptorKind[]): RegistryState<M> {
  const { locale } = useLocale();
  const kindsKey = [...kinds].sort().join(",");
  const key = cacheKey(kinds, locale);
  const [entry, setEntry] = useState<Entry | undefined>(() => cache.get(key));
  const [error, setError] = useState<Error | null>(null);
  const [tick, setTick] = useState(0);

  useEffect(() => {
    const fn = () => setTick((n) => n + 1);
    listeners.add(fn);
    return () => {
      listeners.delete(fn);
    };
  }, []);

  useEffect(() => {
    let live = true;
    const cached = cache.get(key);
    if (cached) setEntry(cached);
    load(kindsKey ? (kindsKey.split(",") as DescriptorKind[]) : [], locale)
      .then((e) => {
        if (!live) return;
        setEntry(e);
        setError(null);
      })
      .catch((e: unknown) => {
        if (live) setError(e instanceof Error ? e : new Error(String(e)));
      });
    return () => {
      live = false;
    };
  }, [key, kindsKey, locale, tick]);

  return useMemo(
    () => ({
      descriptors: (entry?.descriptors ?? []) as Descriptor<M>[],
      loading: !entry && !error,
      error,
      reload: () => {
        cache.delete(key);
        setTick((n) => n + 1);
      },
    }),
    [entry, error, key],
  );
}

/** Test helper: forget every cached registry response. */
export function resetRegistryCache(): void {
  cache.clear();
  inflight.clear();
}
