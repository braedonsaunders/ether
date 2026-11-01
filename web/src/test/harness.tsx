import { act, render } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { vi } from "vitest";
import App from "../App";
import { createFakeServer, type FakeOptions } from "../mock/server";

/** Render the whole app at `path` against a fresh in-memory fake backend. */
export function renderApp(
  path: string,
  opts: FakeOptions = {},
  setup?: (fake: ReturnType<typeof createFakeServer>) => void,
) {
  const fake = createFakeServer(opts);
  setup?.(fake);
  vi.stubGlobal("fetch", vi.fn(fake.fetch));
  const utils = render(
    <MemoryRouter initialEntries={[path]}>
      <App />
    </MemoryRouter>,
  );
  return { ...utils, fake };
}

/**
 * Retry `assert` until it passes. Fake timers advance the visibility draft
 * debounce. Real timers yield one macrotask and jump the animation clock, so a
 * drawer spring finishes without a wall-clock budget.
 */
export async function settleUi(assert: () => void) {
  let last: unknown = new Error("settleUi assertion did not pass");
  const fake = vi.isFakeTimers();
  const steps = fake ? 40 : 100;
  let clock = fake ? 0 : Date.now();
  const now = fake ? null : vi.spyOn(performance, "now").mockImplementation(() => clock);
  try {
    for (let i = 0; i < steps; i++) {
      try {
        assert();
        return;
      } catch (err) {
        last = err;
        if (!fake) clock += 250;
        await act(async () => {
          if (fake) await vi.advanceTimersByTimeAsync(25);
          else await new Promise((resolve) => setTimeout(resolve, 0));
        });
      }
    }
  } finally {
    now?.mockRestore();
  }
  throw last;
}

/** Requests the app made, optionally filtered by method and path prefix. */
export function calls(
  fake: ReturnType<typeof createFakeServer>,
  method: string,
  pathPrefix: string,
) {
  return fake.state.calls.filter((c) => c.method === method && c.path.startsWith(pathPrefix));
}
