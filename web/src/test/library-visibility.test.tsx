import { fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";
import App from "../App";
import { createFakeServer } from "../mock/server";
import { calls, renderApp, settleUi } from "./harness";
import { renderAppWith } from "./routes";

beforeEach(() => {
  vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout", "setInterval", "clearInterval"] });
});

describe("Library visibility", () => {
  it.each([null, undefined])("keeps empty previews usable when items is %s", async (items) => {
    const { fake } = renderAppWith("/catalog/visibility", (_method, path) => {
      if (path === "/catalog/visibility/preview")
        return { total: 63996, hidden: 0, movies: 0, shows: 0, unknownCountry: 16721, items };
    });
    await settleUi(() => {
      expect(screen.getByText("63,996")).toBeInTheDocument();
      expect(screen.getByText("No rules yet. Everything is shown.")).toBeInTheDocument();
    });
    expect(screen.queryByText("Hidden")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Add rule" }));
    expect(screen.getByLabelText("Rule name")).toHaveValue("");
    expect(calls(fake, "PUT", "/config/catalog")).toHaveLength(0);
  });

  it("previews stored rules and saves the catalog draft", async () => {
    const exclusions = [
      {
        name: "Country boundary",
        mediaType: "mixed",
        match: "all",
        rules: [{ field: "country", op: "in", value: ["IN"] }],
      },
    ];
    const fake = createFakeServer();
    (fake.state.config.catalog as { exclusions?: unknown }).exclusions = exclusions;
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = new URL(String(input), "http://localhost");
        const path = url.pathname.replace(/^\/ether\/api/, "");
        const method = (init?.method ?? "GET").toUpperCase();
        let body: unknown;
        if (typeof init?.body === "string" && init.body) body = JSON.parse(init.body);
        if (method === "POST" && path === "/catalog/visibility/preview") {
          const n = ((body as { exclusions?: unknown[] }).exclusions ?? []).length;
          fake.state.calls.push({ method, path: path + url.search, body });
          return new Response(
            JSON.stringify({
              total: 55000,
              hidden: n ? 1200 : 0,
              movies: n ? 800 : 0,
              shows: n ? 400 : 0,
              rules: n ? [{ movies: 800, shows: 400 }] : [],
              items: [],
              shown: [],
            }),
            { status: 200, headers: { "Content-Type": "application/json" } },
          );
        }
        return fake.fetch(input, init);
      }),
    );
    render(
      <MemoryRouter initialEntries={["/catalog/visibility"]}>
        <App />
      </MemoryRouter>,
    );
    await settleUi(() => {
      expect(screen.getByText(/Hide · Country is IN/)).toBeInTheDocument();
      expect(fake.state.calls.filter((c) => c.path.includes("preview")).length).toBeGreaterThan(0);
      expect(screen.getByText("Hidden: 800 movies · 400 shows")).toBeInTheDocument();
      expect(screen.getByText("800 movies · 400 shows")).toBeInTheDocument();
    });
    fireEvent.click(screen.getByRole("button", { name: "Delete rule 1" }));
    await settleUi(() => {
      expect(screen.getByRole("button", { name: "Save changes" })).toBeEnabled();
    });
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await settleUi(() => {
      expect(calls(fake, "PUT", "/config/catalog")).toHaveLength(1);
    });
    expect(calls(fake, "PUT", "/config/catalog")[0]?.body).toMatchObject({
      exclusions: [],
      language: "en-US",
    });
  });

  it("never shows stored rules as empty while adopting them", async () => {
    let flashedEmpty = false;
    const observer = new MutationObserver(() => {
      flashedEmpty ||=
        document.body.textContent?.includes("No rules yet. Everything is shown.") ?? false;
    });
    observer.observe(document.body, { childList: true, subtree: true, characterData: true });
    renderApp("/catalog/visibility", {}, (server) => {
      (server.state.config.catalog as { exclusions?: unknown }).exclusions = [
        {
          name: "Stored group",
          mediaType: "mixed",
          match: "all",
          rules: [{ field: "country", op: "in", value: ["IN"] }],
        },
      ];
    });
    await settleUi(() => {
      expect(screen.getByText(/Hide · Country is IN/)).toBeInTheDocument();
    });
    observer.disconnect();
    expect(flashedEmpty).toBe(false);
  });

  it("keeps a compiler rejection out of the preview", async () => {
    const fake = createFakeServer();
    (fake.state.config.catalog as { exclusions?: unknown }).exclusions = [
      {
        name: "",
        action: "hide",
        mediaType: "mixed",
        match: "all",
        rules: [{ field: "genre", op: "in", value: ["Horror"] }],
      },
    ];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = new URL(String(input), "http://localhost");
        const path = url.pathname.replace(/^\/ether\/api/, "");
        const method = (init?.method ?? "GET").toUpperCase();
        if (method === "POST" && path === "/catalog/visibility/preview") {
          return new Response(
            JSON.stringify({
              error: {
                code: "invalid_filter",
                message: "group 2 needs a name of 1–120 characters",
              },
            }),
            { status: 400, headers: { "Content-Type": "application/json" } },
          );
        }
        return fake.fetch(input, init);
      }),
    );
    render(
      <MemoryRouter initialEntries={["/catalog/visibility"]}>
        <App />
      </MemoryRouter>,
    );
    await settleUi(() => {
      expect(screen.getByText(/Hide · Genre is Horror/)).toBeInTheDocument();
      expect(screen.getByText("Preview isn't available.")).toBeInTheDocument();
    });
    expect(screen.queryByText(/needs a name/)).not.toBeInTheDocument();
    expect(screen.queryByText("Something went wrong")).not.toBeInTheDocument();
    expect(screen.queryByText(/0 titles visible/)).not.toBeInTheDocument();
  });

  it("does not save an unfinished rule", async () => {
    const { fake } = renderApp("/catalog/visibility");
    await settleUi(() => {
      expect(screen.getByText("No rules yet. Everything is shown.")).toBeInTheDocument();
    });
    fireEvent.click(screen.getByRole("button", { name: "Add rule" }));
    expect(screen.getByLabelText("Rule name")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Save changes" })).toBeDisabled();
    expect(calls(fake, "PUT", "/config/catalog")).toHaveLength(0);
  });
  it("shows a stored film-and-TV allowlist as rating levels", async () => {
    renderApp("/catalog/visibility", {}, (server) => {
      (server.state.config.catalog as { exclusions?: unknown }).exclusions = [
        {
          name: "Family",
          mediaType: "mixed",
          match: "all",
          rules: [{ field: "contentRating", op: "notIn", value: ["G", "PG", "TV-G", "TV-PG"] }],
        },
      ];
    });
    await settleUi(() => {
      expect(screen.getByText(/Hide · Content rating is/)).toBeInTheDocument();
      expect(screen.getByText(/G · TV-G · TV-Y/)).toBeInTheDocument();
      expect(screen.getByText(/PG · TV-PG/)).toBeInTheDocument();
    });
    expect(screen.getByText(/PG · TV-PG · TV-Y7/)).toBeInTheDocument();
    expect(screen.queryByText("TV-PG")).not.toBeInTheDocument();
    expect(screen.getByText(/Ratings match by age level/)).toBeInTheDocument();
  });
});
