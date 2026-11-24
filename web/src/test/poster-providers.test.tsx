import { fireEvent, render, screen, waitFor, cleanup } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { afterEach, expect, it, vi } from "vitest";
import { PosterAccounts } from "../pages/settings/PosterAccounts";
import { PosterProviders } from "../pages/metadata/PosterProviders";
import { api, apiFetch } from "../lib/api";
vi.mock("../lib/api", () => ({ api: { getConfig: vi.fn() }, apiFetch: vi.fn() }));
afterEach(cleanup);
it("sets up RPDB and Custom from a fresh config before choosing either in Artwork", async () => {
  let stored: Record<string, unknown> = {};
  vi.mocked(api.getConfig).mockImplementation(
    async () => stored as unknown as Awaited<ReturnType<typeof api.getConfig>>,
  );
  vi.mocked(apiFetch).mockImplementation(async (_url, opts) => {
    const value = JSON.parse(opts!.body as string);
    stored = { posters: value };
    return { value } as never;
  });
  const view = render(
    <MemoryRouter>
      <PosterAccounts locked={false}>
        {(cards, savedNote) => (
          <>
            {cards}
            {savedNote}
          </>
        )}
      </PosterAccounts>
    </MemoryRouter>,
  );
  const reloadNote =
    "Apps reload posters the next time they open a library; some may need to be restarted.";
  const rpdb = await screen.findByRole("switch", { name: "RPDB enabled" });
  await waitFor(() => expect(rpdb).toBeEnabled());
  expect(screen.queryByText(reloadNote)).not.toBeInTheDocument();
  fireEvent.click(rpdb);
  fireEvent.change(screen.getByLabelText("API key"), { target: { value: "rpdb-key" } });
  fireEvent.click(screen.getByRole("button", { name: "Save and enable" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  expect(screen.getByText(reloadNote)).toBeInTheDocument();
  fireEvent.click(screen.getByRole("switch", { name: "Custom posters enabled" }));
  fireEvent.change(screen.getByLabelText("Poster URL template"), {
    target: { value: "https://example.com/{type}/{tmdb_id}" },
  });
  fireEvent.click(screen.getByRole("button", { name: "Save and enable" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  view.unmount();
  render(
    <MemoryRouter>
      <PosterProviders onSaved={() => {}} />
    </MemoryRouter>,
  );
  const picker = await screen.findByRole("button", { name: "Poster source" });
  await waitFor(() => expect(picker).toBeEnabled());
  expect(screen.queryByText(reloadNote)).not.toBeInTheDocument();
  fireEvent.click(picker);
  expect(screen.getAllByRole("option").map((o) => o.textContent)).toEqual([
    "TMDB",
    "Your posters (PosterFlow/folder), then TMDB",
    "RPDB",
    "Custom posters",
  ]);
  fireEvent.click(screen.getByRole("option", { name: "RPDB" }));
  await waitFor(() => expect(picker).toBeEnabled());
  expect(screen.getByText(reloadNote)).toBeInTheDocument();
  for (const source of ["RPDB", "Custom posters"]) {
    fireEvent.click(picker);
    fireEvent.click(screen.getByRole("option", { name: source }));
    await waitFor(() => expect(picker).toBeEnabled());
    expect(picker).toHaveTextContent(source);
  }
  expect(screen.queryByLabelText("API key")).not.toBeInTheDocument();
});

it("opens the setup drawer from a disabled provider's Set up button", async () => {
  vi.mocked(api.getConfig).mockResolvedValue(
    {} as unknown as Awaited<ReturnType<typeof api.getConfig>>,
  );
  render(
    <MemoryRouter>
      <PosterAccounts locked={false}>{(cards) => <>{cards}</>}</PosterAccounts>
    </MemoryRouter>,
  );
  const setUp = await screen.findByRole("button", { name: /Set up RPDB/ });
  await waitFor(() => expect(setUp).toBeEnabled());
  // No dead link to a tab that doesn't exist yet.
  expect(screen.queryByRole("link", { name: /Set up RPDB/ })).not.toBeInTheDocument();
  fireEvent.click(setUp);
  expect(await screen.findByRole("dialog")).toBeInTheDocument();
  expect(screen.getByLabelText("API key")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Save and enable" })).toBeInTheDocument();
});

it("leaves an enabled provider's tab without throwing", async () => {
  vi.mocked(api.getConfig).mockResolvedValue({
    posters: {
      version: 1,
      source: "tmdb",
      accounts: { postersplus: { enabled: true, baseUrl: "https://posters.example" } },
    },
  } as unknown as Awaited<ReturnType<typeof api.getConfig>>);
  const page = (tab: string) => (
    <MemoryRouter>
      <PosterAccounts locked={false} tab={tab}>
        {(cards) => <>{cards}</>}
      </PosterAccounts>
    </MemoryRouter>
  );
  const view = render(page("postersplus"));
  expect(await screen.findByLabelText("Server URL")).toBeInTheDocument();
  view.rerender(page("overview"));
  await waitFor(() => expect(screen.queryByLabelText("Server URL")).not.toBeInTheDocument());
  expect(screen.getByRole("switch", { name: "PostersPlus enabled" })).toBeInTheDocument();
});
