import { toast, UiTextProvider } from "@braedonsaunders/appkit-ui";
import { Globe } from "lucide-react";
import { createContext, useCallback, useContext, useMemo, useState, type ReactNode } from "react";
import { ToolbarPicker, type ToolbarOption } from "../components/ToolbarPicker";
import {
  getLocale,
  loadLocale,
  LOCALES,
  rememberLocale,
  t,
  type CatalogLoader,
  type Locale,
} from "./index";

interface LocaleState {
  locale: Locale;
  setLocale: (locale: Locale) => Promise<void>;
}

const LocaleContext = createContext<LocaleState>({
  locale: getLocale(),
  setLocale: async () => {},
});

/**
 * Holds the active language and hands Ether's translator to AppKit's UI
 * primitives. `children` is a render function, called again on every switch,
 * so the whole tree re-renders in place: component state (unsaved drafts,
 * open drawers) survives, and memos that build copy list `locale` in their
 * dependencies (enforced by the catalogue test).
 */
export function I18nProvider({
  children,
  load,
}: {
  children: (locale: Locale) => ReactNode;
  load?: CatalogLoader;
}) {
  const [locale, setActive] = useState<Locale>(getLocale);
  const setLocale = useCallback(
    async (next: Locale) => {
      try {
        if (!(await loadLocale(next, load))) return; // a later choice superseded this one
      } catch {
        const name = LOCALES.find((l) => l.value === next)?.nativeLabel ?? next;
        toast.error(
          t("Couldn't load {language}. Check your connection and try again.", {
            language: name,
          }),
        );
        return;
      }
      rememberLocale(next);
      setActive(next);
    },
    [load],
  );
  const value = useMemo(() => ({ locale, setLocale }), [locale, setLocale]);
  return (
    <LocaleContext.Provider value={value}>
      <UiTextProvider translate={t}>{children(locale)}</UiTextProvider>
    </LocaleContext.Provider>
  );
}

export function useLocale(): LocaleState {
  return useContext(LocaleContext);
}

/** Language dropdown; each language is listed by its own name. */
export function LanguagePicker({
  iconOnly,
  side = "top",
  className,
}: {
  iconOnly?: boolean;
  side?: "top" | "bottom";
  className?: string;
}) {
  const { locale, setLocale } = useLocale();
  const options = useMemo<ToolbarOption<Locale>[]>(() => {
    const names = new Intl.DisplayNames([locale], { type: "language" });
    return LOCALES.map((l) => {
      const local = names.of(l.value);
      return {
        value: l.value,
        label: l.nativeLabel,
        hint: local && local !== l.nativeLabel ? local : undefined,
        icon: <Globe size={14} />,
      };
    });
  }, [locale]);
  return (
    <ToolbarPicker
      value={locale}
      options={options}
      onChange={(next) => void setLocale(next)}
      ariaLabel={t("Display language")}
      searchPlaceholder={t("Search languages…")}
      iconOnly={iconOnly}
      side={side}
      className={className}
    />
  );
}
