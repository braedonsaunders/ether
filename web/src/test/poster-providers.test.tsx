import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { expect, it, vi } from "vitest";
import { PosterProviders } from "../pages/metadata/PosterProviders";
import { api, apiFetch } from "../lib/api";
vi.mock("../lib/api", () => ({ api: { getConfig: vi.fn() }, apiFetch: vi.fn() }));
it("saves independently and clears keys when changing destination", async () => {
  vi.mocked(api.getConfig).mockResolvedValue({
    posters: {
      enabled: true,
      source: "postersplus",
      baseUrl: "http://lan:8000",
      apiKey: "••••",
      urlTemplate: "",
    },
  } as Awaited<ReturnType<typeof api.getConfig>>);
  vi.mocked(apiFetch).mockResolvedValue({
    value: {
      enabled: true,
      source: "postersplus",
      baseUrl: "http://new:8000",
      apiKey: "",
      urlTemplate: "",
    },
  });
  const saved = vi.fn();
  render(
    <MemoryRouter>
      <PosterProviders onSaved={saved} />
    </MemoryRouter>,
  );
  const configure = screen.getByRole("button", { name: "Configure poster provider" });
  await waitFor(() => expect(configure).toBeEnabled());
  expect(screen.queryByLabelText(/Server URL/)).not.toBeInTheDocument();
  fireEvent.click(configure);
  expect(screen.getAllByText("Poster service connected").length).toBeGreaterThan(0);
  expect(screen.queryByLabelText(/Access key|API key|Poster URL template/)).not.toBeInTheDocument();
  fireEvent.change(await screen.findByLabelText(/Server URL/), {
    target: { value: "http://new:8000" },
  });
  fireEvent.click(screen.getByRole("button", { name: "Save poster provider" }));
  await waitFor(() => expect(saved).toHaveBeenCalledOnce());
  const [path, opts] = vi.mocked(apiFetch).mock.calls[0]!;
  expect(path).toBe("/config/posters");
  expect(JSON.parse(opts!.body as string)).toMatchObject({
    baseUrl: "http://new:8000",
    apiKey: "••••",
  });
  await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  fireEvent.click(configure);
  fireEvent.change(screen.getByLabelText("Source"), { target: { value: "betterposters" } });
  expect(screen.queryByLabelText(/Poster URL template/)).not.toBeInTheDocument();
  fireEvent.change(screen.getByLabelText("Source"), { target: { value: "xrdb" } });
  fireEvent.change(screen.getByLabelText("Server URL"), {
    target: { value: "https://xrdb.example" },
  });
  fireEvent.change(screen.getByLabelText("Profile ID or alias (optional)"), {
    target: { value: "family" },
  });
  expect(screen.queryByLabelText("Access key (optional)")).not.toBeInTheDocument();
});

it("discards provider edits when the drawer is canceled", async () => {
  vi.mocked(apiFetch).mockClear();
  vi.mocked(api.getConfig).mockResolvedValue({
    posters: {
      enabled: false,
      source: "postersplus",
      baseUrl: "http://saved:8000",
      apiKey: "",
      urlTemplate: "",
    },
  } as Awaited<ReturnType<typeof api.getConfig>>);
  render(
    <MemoryRouter>
      <PosterProviders onSaved={vi.fn()} />
    </MemoryRouter>,
  );
  const configure = screen.getByRole("button", { name: "Configure poster provider" });
  await waitFor(() => expect(configure).toBeEnabled());
  fireEvent.click(configure);
  fireEvent.change(await screen.findByLabelText("Server URL"), {
    target: { value: "http://unsaved:8000" },
  });
  fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  expect(apiFetch).not.toHaveBeenCalled();
  expect(screen.getByText("TMDB artwork")).toBeInTheDocument();
  fireEvent.click(configure);
  expect(await screen.findByLabelText("Server URL")).toHaveValue("http://saved:8000");
});
