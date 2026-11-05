import { fireEvent, render, screen, within } from "@testing-library/react";
import { useState } from "react";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import { describe, expect, it, vi } from "vitest";
import type { AvailableRow, HomeLayout } from "../../lib/contract";
import { EMPTY_LAYOUT, setPolicy, setStatuses, type LayerKey } from "../../lib/layout";
import { RowPool } from "./RowPool";

vi.mock("../../lib/extend", async (load) => {
  const actual = await load<typeof import("../../lib/extend")>();
  const id = "plugin:fixture/picks";
  return {
    ...actual,
    useRegistry: (kind: string) => ({
      descriptors:
        kind === "rowSource"
          ? [
              {
                id,
                kind,
                owner: "fixture",
                label: { resolved: "Picks" },
                meta: { hubId: id, collectionSource: id, sections: ["home"] },
              },
            ]
          : [
              {
                id,
                kind,
                owner: "fixture",
                label: { resolved: "Picks" },
                meta: { editor: "plugin", defaults: { params: { genre: "drama" } } },
              },
            ],
      loading: false,
      error: null,
      reload: () => {},
    }),
  };
});
vi.mock("../../lib/api", async (load) => {
  const actual = await load<typeof import("../../lib/api")>();
  return { ...actual, api: { ...actual.api, rowInsights: async () => [] } };
});
function Destination() {
  const location = useLocation();
  return <output>{JSON.stringify(location.state)}</output>;
}
describe("plugin Home row sources", () => {
  it("starts an enabled collection with the plugin defaults and Home target", () => {
    render(
      <MemoryRouter initialEntries={["/"]}>
        <Routes>
          <Route
            path="/"
            element={
              <RowPool
                draft={{ global: [], viewers: [] }}
                onChange={() => {}}
                layer="global"
                section="home"
                viewers={[]}
                available={[]}
                focusRow={null}
                onFocusRow={() => {}}
              />
            }
          />
          <Route path="/collections/new" element={<Destination />} />
        </Routes>
      </MemoryRouter>,
    );
    fireEvent.click(screen.getByRole("button", { name: "Create collection" }));
    const state = JSON.parse(screen.getByText(/plugin:fixture/).textContent!);
    expect(state.spec).toMatchObject({
      enabled: true,
      name: "Picks",
      targets: { homeRow: true },
      sources: [{ kind: "plugin:fixture/picks", params: { genre: "drama" } }],
    });
  });
});

function Harness({
  initial,
  available,
  onDraft,
  layer = "global",
}: {
  initial: HomeLayout;
  available: AvailableRow[];
  onDraft?: (d: HomeLayout) => void;
  layer?: LayerKey;
}) {
  const [draft, setDraft] = useState(initial);
  const [focus, setFocus] = useState<string | null>(null);
  return (
    <MemoryRouter>
      <RowPool
        draft={draft}
        onChange={(d) => {
          setDraft(d);
          onDraft?.(d);
        }}
        layer={layer}
        section="home"
        viewers={[]}
        available={available}
        focusRow={focus}
        onFocusRow={setFocus}
      />
    </MemoryRouter>
  );
}

const picks: AvailableRow[] = [
  { hubId: "trending", title: "Trending", kind: "trending", description: "", group: "picks" },
  { hubId: "top-10", title: "Top 10", kind: "top-10", description: "", group: "picks" },
  { hubId: "fresh", title: "Fresh", kind: "fresh", description: "", group: "picks" },
];
const collections: AvailableRow[] = Array.from({ length: 20 }, (_, i) => ({
  hubId: `collection:c${i}`,
  title: `Collection ${i}`,
  kind: `collection:c${i}`,
  description: "",
  group: "collections",
}));
const rowsOf = (d: HomeLayout) => d.global.find((p) => p.section === "home")?.rows ?? [];

describe("bulk row editing", () => {
  it("sets the status of every selected row at once", () => {
    let last: HomeLayout | undefined;
    render(<Harness initial={EMPTY_LAYOUT} available={picks} onDraft={(d) => (last = d)} />);
    fireEvent.click(screen.getByRole("checkbox", { name: "Select Trending" }));
    fireEvent.click(screen.getByRole("checkbox", { name: "Select Fresh" }));
    fireEvent.click(screen.getByRole("button", { name: "Set the selected rows to Off" }));
    expect(rowsOf(last!)).toEqual([
      { row: "trending", status: "off" },
      { row: "fresh", status: "off" },
    ]);
    expect(screen.queryByRole("toolbar", { name: "Selected rows" })).toBeNull();
  });

  it("selects a whole group and resets it to default", () => {
    let last: HomeLayout | undefined;
    const initial = setStatuses(EMPTY_LAYOUT, "global", "home", ["trending", "top-10"], "always");
    render(<Harness initial={initial} available={picks} onDraft={(d) => (last = d)} />);
    fireEvent.click(screen.getByRole("checkbox", { name: "Select every row in Picks for you" }));
    fireEvent.click(screen.getByRole("button", { name: "Set the selected rows back to default" }));
    expect(rowsOf(last!)).toEqual([]);
  });

  it("changes a row's status inline, without opening it", () => {
    let last: HomeLayout | undefined;
    render(<Harness initial={EMPTY_LAYOUT} available={picks} onDraft={(d) => (last = d)} />);
    const status = screen.getByRole("radiogroup", { name: "Status of Top 10" });
    fireEvent.click(within(status).getByRole("radio", { name: "Always" }));
    expect(rowsOf(last!)).toEqual([{ row: "top-10", status: "always" }]);
  });

  it("clears the selection when the layer or what the list shows changes", () => {
    const { rerender } = render(<Harness initial={EMPTY_LAYOUT} available={picks} />);
    fireEvent.click(screen.getByRole("checkbox", { name: "Select Trending" }));
    expect(screen.getByRole("toolbar", { name: "Selected rows" })).toBeTruthy();
    const filter = screen.getByRole("radiogroup", { name: "Show rows by status" });
    fireEvent.click(within(filter).getByRole("radio", { name: "Eligible" }));
    expect(screen.queryByRole("toolbar", { name: "Selected rows" })).toBeNull();
    fireEvent.click(screen.getByRole("checkbox", { name: "Select Trending" }));
    rerender(<Harness initial={EMPTY_LAYOUT} available={picks} layer={7} />);
    expect(screen.queryByRole("toolbar", { name: "Selected rows" })).toBeNull();
  });

  it("starts a viewer's bulk overrides from Everyone's policy, like a single edit", () => {
    let last: HomeLayout | undefined;
    const initial = setPolicy(EMPTY_LAYOUT, "global", "home", {
      row: "top-10",
      status: "always",
      priority: 2.5,
      maxPerPage: 1,
    });
    render(<Harness initial={initial} available={picks} layer={7} onDraft={(d) => (last = d)} />);
    fireEvent.click(screen.getByRole("checkbox", { name: "Select Top 10" }));
    fireEvent.click(screen.getByRole("checkbox", { name: "Select Fresh" }));
    fireEvent.click(screen.getByRole("button", { name: "Set the selected rows to Off" }));
    const viewer = last!.viewers.find((v) => v.userId === 7)!.pages[0]!.rows;
    expect(viewer).toEqual([
      { row: "top-10", status: "off", priority: 2.5, maxPerPage: 1 },
      { row: "fresh", status: "off" },
    ]);
  });

  it("filters the catalogue by status", () => {
    const initial = setStatuses(EMPTY_LAYOUT, "global", "home", ["fresh"], "off");
    render(<Harness initial={initial} available={picks} />);
    const filter = screen.getByRole("radiogroup", { name: "Show rows by status" });
    fireEvent.click(within(filter).getByRole("radio", { name: "Off" }));
    expect(screen.getByRole("listitem", { name: "Row Fresh" })).toBeTruthy();
    expect(screen.queryByRole("listitem", { name: "Row Trending" })).toBeNull();
  });
});

describe("collection rows", () => {
  it("starts a long Collections group folded", () => {
    render(<Harness initial={EMPTY_LAYOUT} available={[...picks, ...collections]} />);
    const toggle = screen.getByRole("button", { name: /^Collections \d+ rows/ });
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    expect(screen.queryByRole("listitem", { name: "Row Collection 3" })).toBeNull();
    fireEvent.click(toggle);
    expect(screen.getByRole("listitem", { name: "Row Collection 3" })).toBeTruthy();
  });

  it("can't fold a group a search is holding open", () => {
    render(<Harness initial={EMPTY_LAYOUT} available={[...picks, ...collections]} />);
    fireEvent.change(screen.getByRole("textbox", { name: "Filter rows" }), {
      target: { value: "Collection" },
    });
    const toggle = screen.getByRole("button", { name: /^Collections \d+ rows/ });
    expect(toggle.getAttribute("aria-expanded")).toBe("true");
    expect((toggle as HTMLButtonElement).disabled).toBe(true);
  });

  it("deselects a group's rows when it is folded", () => {
    render(<Harness initial={EMPTY_LAYOUT} available={picks} />);
    fireEvent.click(screen.getByRole("checkbox", { name: "Select Trending" }));
    fireEvent.click(screen.getByRole("button", { name: /^Picks for you/ }));
    expect(screen.queryByRole("toolbar", { name: "Selected rows" })).toBeNull();
  });

  it("turns a whole Collections group off row by row, without a pattern", () => {
    let last: HomeLayout | undefined;
    const few = collections.slice(0, 3);
    render(<Harness initial={EMPTY_LAYOUT} available={few} onDraft={(d) => (last = d)} />);
    fireEvent.click(screen.getByRole("checkbox", { name: "Select every row in Collections" }));
    fireEvent.click(screen.getByRole("button", { name: "Set the selected rows to Off" }));
    expect(rowsOf(last!)).toEqual([
      { row: "collection:c0", status: "off" },
      { row: "collection:c1", status: "off" },
      { row: "collection:c2", status: "off" },
    ]);
    expect(screen.queryByRole("listitem", { name: "Row collection:*" })).toBeNull();
  });

  it("shows a row a pattern reaches with the pattern's status", () => {
    render(
      <Harness
        initial={setStatuses(EMPTY_LAYOUT, "global", "home", ["collection:*"], "off")}
        available={collections.slice(0, 3)}
      />,
    );
    const c2 = screen.getByRole("listitem", { name: "Row Collection 2" });
    expect(within(c2).getByText("via collection:*")).toBeTruthy();
    expect(within(c2).getByRole("radio", { name: "Off" }).getAttribute("aria-checked")).toBe(
      "true",
    );
  });

  it("shows what a pattern reaches and removes it in one click", () => {
    let last: HomeLayout | undefined;
    render(
      <Harness
        initial={setStatuses(EMPTY_LAYOUT, "global", "home", ["collection:*"], "off")}
        available={collections}
        onDraft={(d) => (last = d)}
      />,
    );
    const rule = screen.getByRole("listitem", { name: "Row collection:*" });
    expect(within(rule).getByText("Applies to 20 rows")).toBeTruthy();
    fireEvent.click(within(rule).getByRole("button", { name: "Remove the rule collection:*" }));
    expect(rowsOf(last!)).toEqual([]);
  });
});
