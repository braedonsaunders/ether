import { fireEvent, screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { renderAppWith } from "./routes";
import { calls, settleUi } from "./harness";

beforeEach(() => {
  vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout", "setInterval", "clearInterval"] });
});

describe("Library visibility", () => {
  it.each([null, undefined])("keeps empty previews usable when items is %s", async (items) => {
    const { fake } = renderAppWith("/catalog/visibility", (_method, path) => {
      if (path === "/config/catalog") return { value: {} };
      if (path === "/catalog/visibility/preview")
        return { total: 63996, hidden: 0, movies: 0, shows: 0, unknownCountry: 16721, items };
    });
    await settleUi(() => {
      expect(
        screen.getByText(
          "63,996 catalogue titles remain before release, availability and viewer restrictions.",
        ),
      ).toBeInTheDocument();
    });
    expect(screen.getByText("No custom exclusions")).toBeInTheDocument();
    expect(screen.queryByText("Example matches")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Add exclusion group" }));
    expect(screen.getByLabelText("Group 1 name")).toBeInTheDocument();
    expect(calls(fake, "PUT", "/config/catalog")).toHaveLength(0);
  });
  it("previews country rules locally and saves only the exclusions patch", async () => {
    const exclusions = [
      {
        name: "Country boundary",
        mediaType: "mixed",
        match: "all",
        rules: [{ field: "country", op: "in", value: ["IN"] }],
      },
    ];
    const { fake } = renderAppWith("/catalog/visibility", (method, path, body) => {
      if (method === "GET" && path === "/config/catalog") return { value: { exclusions } };
      if (method === "POST" && path === "/catalog/visibility/preview")
        return {
          total: 55000,
          hidden: (body as { exclusions: unknown[] }).exclusions.length ? 1200 : 0,
          movies: 1000,
          shows: 200,
          unknownCountry: 300,
          items: [],
        };
    });
    await settleUi(() => {
      expect(screen.getByText("Your catalogue. Your boundaries.")).toBeInTheDocument();
      expect(screen.getByText("1,200")).toBeInTheDocument();
    });
    expect(screen.getByText("Country is not language")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Delete group 1" }));
    await settleUi(() => {
      expect(screen.getByRole("button", { name: "Save visibility" })).toBeEnabled();
    });
    fireEvent.click(screen.getByRole("button", { name: "Save visibility" }));
    await settleUi(() => {
      expect(calls(fake, "PUT", "/config/catalog")).toHaveLength(1);
    });
    expect(calls(fake, "PUT", "/config/catalog")[0]?.body).toEqual({ exclusions: [] });
    expect(calls(fake, "GET", "/catalog/search")).toHaveLength(0);
  });
  it("never shows stored groups as empty while adopting them", async () => {
    let flashedEmpty = false;
    const observer = new MutationObserver(() => {
      flashedEmpty ||= document.body.textContent?.includes("No custom exclusions") ?? false;
    });
    observer.observe(document.body, { childList: true, subtree: true, characterData: true });
    const exclusions = [
      {
        name: "Stored group",
        mediaType: "mixed",
        match: "all",
        rules: [{ field: "country", op: "in", value: ["IN"] }],
      },
    ];
    renderAppWith("/catalog/visibility", (method, path) => {
      if (method === "GET" && path === "/config/catalog") return { value: { exclusions } };
      if (path === "/catalog/visibility/preview")
        return { total: 500, hidden: 1, movies: 1, shows: 0, unknownCountry: 0, items: [] };
    });
    await settleUi(() => {
      expect(screen.getByDisplayValue("Stored group")).toBeInTheDocument();
    });
    observer.disconnect();
    expect(flashedEmpty).toBe(false);
  });
  it("does not enable an example exclusion or save an incomplete group", async () => {
    const { fake } = renderAppWith("/catalog/visibility", (_method, path) => {
      if (path === "/config/catalog") return { value: {} };
      if (path === "/catalog/visibility/preview")
        return { total: 500, hidden: 0, movies: 0, shows: 0, unknownCountry: 1, items: [] };
    });
    await settleUi(() => {
      expect(screen.getByText("No custom exclusions")).toBeInTheDocument();
    });
    fireEvent.click(screen.getByRole("button", { name: "Add exclusion group" }));
    expect(screen.getByLabelText("Group 1 name")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Save visibility" })).toBeDisabled();
    expect(calls(fake, "PUT", "/config/catalog")).toHaveLength(0);
  });
  it("shows a stored film-and-TV allowlist as rating levels", async () => {
    const exclusions = [
      {
        name: "Family",
        mediaType: "mixed",
        match: "all",
        rules: [{ field: "contentRating", op: "notIn", value: ["G", "PG", "TV-G", "TV-PG"] }],
      },
    ];
    renderAppWith("/catalog/visibility", (method, path) => {
      if (method === "GET" && path === "/config/catalog") return { value: { exclusions } };
      if (path === "/catalog/visibility/preview")
        return { total: 500, hidden: 120, movies: 100, shows: 20, unknownCountry: 0, items: [] };
    });
    await settleUi(() => {
      expect(screen.getByDisplayValue("Family")).toBeInTheDocument();
      expect(screen.getByText("G · TV-G · TV-Y")).toBeInTheDocument();
    });
    expect(screen.getByText("PG · TV-PG")).toBeInTheDocument();
    expect(screen.queryByText("TV-PG")).not.toBeInTheDocument();
    expect(screen.getByText("Ratings match by age level")).toBeInTheDocument();
  });
});
