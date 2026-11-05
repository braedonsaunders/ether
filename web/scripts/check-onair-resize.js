// Run against mock mode with the host playwright-cli:
// playwright-cli -s=w23 run-code "$(cat web/scripts/check-onair-resize.js)"
async function checkOnAirResize(page) {
  await page.emulateMedia({ reducedMotion: "reduce" });
  await page.setViewportSize({ width: 1440, height: 1000 });
  const origin = await page.evaluate(() => location.origin);
  await page.goto(origin + "/ether/livetv?channel=v-80s");
  const timeline = page.locator(".schedule-timeline");
  await timeline.locator(".schedule-element").first().waitFor();
  // Six hours crosses the title/subtitle threshold as the native drawer expands.
  while (Number(await timeline.getAttribute("data-visible-hours")) < 6)
    await page.getByRole("button", { name: "Zoom out", exact: true }).click();
  const before = Number(await timeline.getAttribute("data-viewport-width"));
  const labels = await timeline
    .locator("[data-label-detail]")
    .evaluateAll((nodes) => nodes.map((n) => n.getAttribute("data-label-detail")));
  await page.getByRole("button", { name: "Fullscreen", exact: true }).click();
  await page.waitForFunction(
    (w) =>
      Number(document.querySelector(".schedule-timeline")?.getAttribute("data-viewport-width")) >
      innerWidth - 100,
    before,
  );
  const expanded = Number(await timeline.getAttribute("data-viewport-width"));
  const expandedLabels = await timeline
    .locator("[data-label-detail]")
    .evaluateAll((nodes) => nodes.map((n) => n.getAttribute("data-label-detail")));
  if (expanded <= before || JSON.stringify(labels) === JSON.stringify(expandedLabels))
    throw Error("Timeline layout did not recompute");
  await page.getByRole("button", { name: "Exit fullscreen", exact: true }).click();
  await page.waitForFunction(
    (w) =>
      Number(document.querySelector(".schedule-timeline")?.getAttribute("data-viewport-width")) < w,
    expanded,
  );
  await page.getByRole("button", { name: "Close", exact: true }).click();
  await page.goto(origin + "/ether/livetv/looks");
  await page
    .getByRole("button", { name: /NEON 84/ })
    .first()
    .click();
  const stage = page.getByLabel("On-air stage", { exact: true });
  await stage.waitFor();
  await page.waitForFunction(() => {
    const c = document.querySelector('canvas[aria-label="On-air stage"]');
    return (
      c && Math.abs(c.width - c.getBoundingClientRect().width * Math.min(2, devicePixelRatio)) < 2
    );
  });
  const oldSize = await stage.evaluate((c) => ({
    width: c.width,
    box: c.getBoundingClientRect().width,
  }));
  await page.getByRole("button", { name: "Fullscreen", exact: true }).click();
  await page.waitForFunction((old) => {
    const c = document.querySelector('canvas[aria-label="On-air stage"]');
    return (
      c &&
      c.width > old &&
      Math.abs(
        document.querySelector('[role="dialog"]').getBoundingClientRect().width - innerWidth,
      ) < 1
    );
  }, oldSize.width);
  const newSize = await stage.evaluate((c) => ({
    width: c.width,
    box: c.getBoundingClientRect().width,
  }));
  if (newSize.box <= oldSize.box) throw Error("Stage did not resize");
  await page.getByRole("button", { name: "Exit fullscreen", exact: true }).click();
  await page.waitForFunction(
    (old) => document.querySelector('canvas[aria-label="On-air stage"]')?.width === old,
    oldSize.width,
  );
  return { timeline: { before, expanded }, stage: { before: oldSize, expanded: newSize } };
}
