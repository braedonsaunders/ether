import { routeApiImagesThroughFetch } from "../lib/api";
import { createFakeServer } from "./server";

/** Route same-origin /ether/api requests to the in-browser fake (mock mode only). */
export function installMockBackend(): void {
  // `?cluster` previews the owner UI as a node of a cluster.
  const params = new URLSearchParams(window.location.search);
  const cluster = params.has("cluster");
  // `?startup=migrating` (or `failed:database_corrupt`, …) previews the
  // startup page; `?viewers=2` puts viewers on the restart drawers;
  // `?opsPace=` slows a simulated restart down (milliseconds per step);
  // `?damaged` reports a damaged database; `?restore=restored|failed|pending`
  // seeds the Backups page's restore report.
  const fake = createFakeServer({
    latencyMs: 120,
    cluster,
    startup: params.get("startup") ?? undefined,
    viewers: Number(params.get("viewers") ?? 0),
    opsPace: params.has("opsPace") ? Number(params.get("opsPace")) : undefined,
    damagedDatabase: params.has("damaged"),
    cacheRestart: params.has("cacheRestart"),
    restore: params.get("restore") ?? undefined,
  });
  const real = window.fetch.bind(window);
  window.fetch = (input: RequestInfo | URL, init?: RequestInit) => {
    const url = new URL(
      input instanceof Request ? input.url : String(input),
      window.location.origin,
    );
    if (url.origin === window.location.origin && url.pathname.startsWith("/ether/api/"))
      return fake.fetch(url.toString(), init);
    return real(input, init);
  };
  // Generated artwork (/ether/api/collections/{id}/artwork) must come from the fake too.
  routeApiImagesThroughFetch(true);
  console.info("[ether] mock backend active: all /ether/api calls are served in the browser.");
}
