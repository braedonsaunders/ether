import { act, renderHook, waitFor } from "@testing-library/react";
import { expect, it, vi } from "vitest";
import { apiFetch } from "../lib/api";
import { useDiscoverySettings } from "../pages/viewers/sections/WatchlistDefaults";

vi.mock("../lib/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../lib/api")>()),
  apiFetch: vi.fn(),
}));

it("saves only the keys this page changed on top of the current settings", async () => {
  let stored: Record<string, unknown> = { a: 1, b: "x" };
  vi.mocked(apiFetch).mockImplementation(async (_url, opts) => {
    if (opts?.method === "PUT") stored = JSON.parse(opts.body as string);
    return structuredClone(stored) as never;
  });
  const { result } = renderHook(() => useDiscoverySettings());
  await waitFor(() => expect(result.current.draft).toBeTruthy());
  // Another page saves b after this page loaded.
  stored = { a: 1, b: "y" };
  act(() => result.current.setDraft({ ...(result.current.draft as object), a: 2 } as never));
  await act(() => result.current.save("saved"));
  expect(stored).toEqual({ a: 2, b: "y" });
});
