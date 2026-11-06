// Browser check for the profile "Evaluation mode" setting (mock backend, test data only).
//
//   cd web && pnpm exec vite --mode mock --port 5199 --host 127.0.0.1 &
//   PLAYWRIGHT_BROWSERS_PATH=$PWD/.cache/ms-playwright \
//     node scripts/playwright/evaluation-mode.mjs   # run from the repo root
//
// Playwright is installed under .cache/pw (not a dependency of the repo).
// Screenshots go to $BB_THREAD_STORAGE (or ./.cache/shots).
import { createRequire } from "node:module";
import { mkdirSync } from "node:fs";
import path from "node:path";

const require = createRequire(path.resolve(".cache/pw/package.json"));
const { chromium } = require("playwright");

const base = process.env.ETHER_UI ?? "http://127.0.0.1:5199";
const out = process.env.BB_THREAD_STORAGE ?? path.resolve(".cache/shots");
mkdirSync(out, { recursive: true });

let failures = 0;
function check(name, ok, extra = "") {
  console.log(`${ok ? "PASS" : "FAIL"} ${name}${extra ? ` (${extra})` : ""}`);
  if (!ok) failures++;
}

const browser = await chromium.launch();
async function run(label, viewport) {
  const page = await browser.newPage({ viewport });
  const errors = [];
  page.on("pageerror", (e) => errors.push(String(e)));
  await page.goto(`${base}/quality`, { waitUntil: "networkidle" });

  // Import a TRaSH profile through the mock backend (the same endpoint the importer uses), so
  // the profile carries `import` and a quality order. The mock keeps state in the page, so
  // reach the Quality list again by navigating inside the app rather than reloading.
  const importedName = await page.evaluate(async () => {
    const api = "/ether/api/releases";
    const list = await (await fetch(`${api}/trash/radarr/profiles`)).json();
    const res = await fetch(`${api}/quality-import`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ app: "radarr", source: "trash", profile: list.items[0].id }),
    });
    const body = await res.json();
    return body?.profile?.name ?? body?.name ?? "";
  });
  check(`${label}: imported a TRaSH profile through the mock importer`, importedName !== "", importedName);
  // In-app navigation without the sidebar (collapsed at narrow widths): react-router follows popstate.
  const navigate = (to) =>
    page.evaluate((path) => {
      history.pushState({}, "", path);
      window.dispatchEvent(new PopStateEvent("popstate"));
    }, to);
  await navigate("/ether/");
  await page.waitForTimeout(500);
  await navigate("/ether/quality");
  await page.getByRole("button", { name: /^Edit(?! scores)/ }).first().waitFor();

  // The mock's Movies list is 4K, 1080p, then the imported profile; each row has an Edit button.
  const rows = { "4K": 0, "1080p": 1, [importedName]: 2 };
  async function openProfile(name) {
    await page.getByRole("button", { name: /^Edit(?! scores)/ }).nth(rows[name]).click();
    await page.getByRole("radiogroup", { name: "Evaluation mode" }).waitFor();
    await page.waitForTimeout(800); // let the drawer finish sliding in
  }
  const group = () => page.getByRole("radiogroup", { name: "Evaluation mode" });
  const selected = async () => {
    const checked = await group().getByRole("radio", { checked: true }).first().innerText();
    return checked.trim();
  };

  // A manual profile defaults to Ether mode.
  await openProfile("1080p");
  check(`${label}: manual profile defaults to Ether`, (await selected()) === "Ether", await selected());
  await page.screenshot({ path: path.join(out, `evaluation-mode-${label}-manual-ether.png`), fullPage: true });
  check(`${label}: Ether explanation shown`, await page.getByText("Ether's own scoring").isVisible());

  // Switch to Servarr; explanation changes.
  await group().getByRole("radio", { name: "Servarr" }).click({ force: true });
  check(`${label}: Servarr selected`, (await selected()) === "Servarr");
  check(`${label}: Servarr explanation shown`, await page.getByText("Scores and orders releases like Radarr and Sonarr").isVisible());
  await page.screenshot({ path: path.join(out, `evaluation-mode-${label}-manual-servarr.png`), fullPage: true });

  // Save and reopen: the choice persists.
  await page.getByRole("button", { name: "Save profile" }).click();
  check(`${label}: save confirmed`, await page.getByText("Profile saved").waitFor({ timeout: 5000 }).then(() => true, () => false));
  await page.waitForTimeout(500);
  await page.keyboard.press("Escape");
  await page.waitForTimeout(300);
  await openProfile("1080p");
  check(`${label}: choice persisted after save`, (await selected()) === "Servarr", await selected());

  // An imported profile with a quality order defaults to Servarr.
  await page.keyboard.press("Escape");
  await page.waitForTimeout(300);
  await openProfile(importedName);
  check(`${label}: imported profile defaults to Servarr`, (await selected()) === "Servarr", await selected());
  await page.screenshot({ path: path.join(out, `evaluation-mode-${label}-imported-servarr.png`), fullPage: true });
  await group().getByRole("radio", { name: "Ether" }).click({ force: true });
  check(`${label}: imported profile can opt out to Ether`, (await selected()) === "Ether");
  await page.screenshot({ path: path.join(out, `evaluation-mode-${label}-imported-ether.png`), fullPage: true });

  // No horizontal overflow of the page.
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
  check(`${label}: no horizontal page overflow`, overflow <= 1, `${overflow}px`);
  check(`${label}: no page errors`, errors.length === 0, errors.join("; "));
  await page.close();
}

await run("desktop", { width: 1280, height: 900 });
await run("narrow", { width: 390, height: 844 });
await browser.close();
console.log(failures ? `${failures} check(s) failed` : "all checks passed");
process.exit(failures ? 1 : 0);
