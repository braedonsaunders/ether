// Translation workflow for the owner console. Run from web/:
//   pnpm i18n status                 coverage per locale
//   pnpm i18n sync                   rewrite locales/en.json and drop stale catalogue keys
//   pnpm i18n todo <locale> [n]      untranslated messages as JSON keyed by the English (first n)
//   pnpm i18n merge <locale> <file>  validate and merge a JSON file keyed by the English
//   pnpm i18n audit [files…]         likely user-visible English not wrapped in t()
//   pnpm i18n dynamic                t()/tx()/tn() calls with non-literal messages
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { systemMessageKey } from "@braedonsaunders/appkit-i18n";
import {
  audit,
  checkCatalog,
  extract,
  formatCatalog,
  keyed,
  localeCodes,
  localesDir,
  problems,
  readCatalog,
  sourceCatalog,
  sourceFiles,
} from "./i18n-lib.mjs";

const [command, ...args] = process.argv.slice(2);

function writeCatalog(locale, catalog) {
  mkdirSync(localesDir, { recursive: true });
  writeFileSync(path.join(localesDir, `${locale}.json`), formatCatalog(catalog));
}

function requireLocale(locale) {
  if (!localeCodes().includes(locale)) {
    console.error(
      `unknown locale ${JSON.stringify(locale)}; supported: ${localeCodes().join(", ")}`,
    );
    process.exit(2);
  }
}

function sync(messages) {
  writeCatalog("en", sourceCatalog(messages));
  const byKey = keyed(messages);
  for (const locale of localeCodes()) {
    const catalog = readCatalog(locale);
    const kept = Object.fromEntries(Object.entries(catalog).filter(([k]) => byKey.has(k)));
    const dropped = Object.keys(catalog).length - Object.keys(kept).length;
    if (dropped) console.log(`${locale}: dropped ${dropped} stale`);
    writeCatalog(locale, kept);
  }
}

switch (command) {
  case "status": {
    const { messages } = extract();
    console.log(`${messages.size} messages`);
    let bad = false;
    for (const locale of localeCodes()) {
      const { missing, stale, invalid } = checkCatalog(locale, messages);
      bad ||= missing.length + stale.length + invalid.length > 0;
      console.log(
        `${locale.padEnd(6)} ${String(messages.size - missing.length).padStart(5)}/${messages.size}` +
          (missing.length ? `  missing ${missing.length}` : "") +
          (stale.length ? `  stale ${stale.length}` : "") +
          (invalid.length ? `  invalid ${invalid.length}` : ""),
      );
      for (const p of invalid.slice(0, 10)) console.log(`         ${p}`);
    }
    process.exit(bad ? 1 : 0);
  }
  case "sync": {
    sync(extract().messages);
    break;
  }
  case "todo": {
    const [locale, limit] = args;
    requireLocale(locale);
    const { messages } = extract();
    const { missing } = checkCatalog(locale, messages);
    const categories = new Intl.PluralRules(locale).resolvedOptions().pluralCategories;
    const out = {};
    for (const source of missing.slice(0, limit ? Number(limit) : undefined)) {
      const one = messages.get(source).one;
      out[source] =
        one === undefined
          ? ""
          : Object.fromEntries(categories.map((c) => [c, c === "one" ? one : source]));
    }
    console.log(JSON.stringify(out, null, 2));
    console.error(`${Object.keys(out).length} of ${missing.length} missing for ${locale}`);
    break;
  }
  case "merge": {
    const [locale, file] = args;
    requireLocale(locale);
    const incoming = JSON.parse(readFileSync(path.resolve(file), "utf8"));
    const { messages } = extract();
    const catalog = readCatalog(locale);
    let merged = 0;
    const rejected = [];
    for (const [source, value] of Object.entries(incoming)) {
      const message = messages.get(source);
      if (!message) {
        rejected.push(`${JSON.stringify(source)}: not a source message`);
        continue;
      }
      const found = problems(locale, { source, ...message }, value);
      if (found.length) {
        rejected.push(`${JSON.stringify(source)}: ${found.join(", ")}`);
        continue;
      }
      catalog[systemMessageKey(source)] = value;
      merged++;
    }
    writeCatalog(locale, catalog);
    const { missing } = checkCatalog(locale, messages);
    console.log(
      `${locale}: merged ${merged}, rejected ${rejected.length}, still missing ${missing.length}`,
    );
    for (const r of rejected) console.log(`  rejected ${r}`);
    process.exit(rejected.length ? 1 : 0);
  }
  case "audit": {
    const files = args.length ? args.map((f) => path.resolve(f)) : sourceFiles();
    const hits = audit(files);
    for (const h of hits) console.log(`${h.ref}  ${JSON.stringify(h.text)}`);
    console.error(`${hits.length} possible untranslated strings`);
    break;
  }
  case "dynamic": {
    for (const ref of extract().dynamic) console.log(ref);
    break;
  }
  default:
    console.error(
      readFileSync(new URL(import.meta.url), "utf8")
        .split("\n")
        .slice(0, 8)
        .join("\n"),
    );
    process.exit(2);
}
