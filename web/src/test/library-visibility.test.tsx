import { fireEvent, screen, waitFor } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { renderAppWith } from "./routes";
import { calls } from "./harness";

describe("Library visibility", () => {
  it.each([null, undefined])("keeps empty previews usable when items is %s", async (items) => {
    const { fake } = renderAppWith("/catalog/visibility", (_method, path) => {
      if (path === "/config/catalog") return { value: {} };
      if (path === "/catalog/visibility/preview")
        return { total: 63996, hidden: 0, movies: 0, shows: 0, unknownCountry: 16721, items };
    });
    await screen.findByText(
      "63,996 catalogue titles remain before release, availability and viewer restrictions.",
    );
    expect(screen.getByText("No custom exclusions")).toBeInTheDocument();
    expect(screen.queryByText("Example matches")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Add exclusion group" }));
    await screen.findByLabelText("Group 1 name");
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
    await screen.findByText("Your catalogue. Your boundaries.");
    await screen.findByText("1,200");
    expect(screen.getByText("Country is not language")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Delete group 1" }));
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Save visibility" })).toBeEnabled(),
    );
    fireEvent.click(screen.getByRole("button", { name: "Save visibility" }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/catalog")).toHaveLength(1));
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
    expect(await screen.findByDisplayValue("Stored group")).toBeInTheDocument();
    observer.disconnect();
    expect(flashedEmpty).toBe(false);
  });
  it("does not enable an example exclusion or save an incomplete group", async () => {
    const { fake } = renderAppWith("/catalog/visibility", (_method, path) => {
      if (path === "/config/catalog") return { value: {} };
      if (path === "/catalog/visibility/preview")
        return { total: 500, hidden: 0, movies: 0, shows: 0, unknownCountry: 1, items: [] };
    });
    await screen.findByText("No custom exclusions");
    fireEvent.click(screen.getByRole("button", { name: "Add exclusion group" }));
    await screen.findByLabelText("Group 1 name");
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
    expect(await screen.findByDisplayValue("Family")).toBeInTheDocument();
    expect(await screen.findByText("G · TV-G · TV-Y")).toBeInTheDocument();
    expect(screen.getByText("PG · TV-PG · TV-Y7")).toBeInTheDocument();
    expect(screen.queryByText("TV-PG")).not.toBeInTheDocument();
    expect(screen.getByText("Ratings match by age level")).toBeInTheDocument();
  });
});
