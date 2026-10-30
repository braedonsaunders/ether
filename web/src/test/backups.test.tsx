import { ConfirmRoot } from "@braedonsaunders/appkit-ui";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { setToken } from "../lib/api";
import { BackupsAndImport } from "../pages/settings/Backups";

function backend() {
  const calls: { url: string; init?: RequestInit }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      calls.push({ url, init });
      let data: unknown = {};
      if (url.endsWith("/users"))
        data = {
          users: [
            { id: 7, title: "Sam" },
            { id: 12, title: "Alex" },
          ],
        };
      if (url.endsWith("/adoption/backups/"))
        data = {
          snapshots: [
            { name: "ether-2026-10-18.tar.gz", size: 4 * 1048576, created: "2026-10-18T03:00:00Z" },
          ],
          settings: { intervalHours: 24, retain: 7 },
          warning: "",
        };
      if (url.endsWith("/migration/preview"))
        data = {
          id: "plan-1",
          imported: false,
          items: [{ title: "A remembered film", key: 7, views: 1 }],
          lists: [],
          warnings: [],
        };
      return new Response(JSON.stringify(data), { status: 200 });
    }),
  );
  return calls;
}

async function choose(label: string, option: string) {
  fireEvent.click(screen.getByRole("button", { name: label }));
  fireEvent.click(await screen.findByRole("option", { name: option }));
}

describe("Backups & import", () => {
  it("previews a Plex import inline and waits for the explicit import", async () => {
    const calls = backend();
    render(<BackupsAndImport tab="import" />);
    await waitFor(() => expect(calls.some((c) => c.url.endsWith("/users"))).toBe(true));
    await choose("Ether viewer", "Alex");
    fireEvent.change(screen.getByLabelText("Plex server URL"), {
      target: { value: "http://source.example:32400" },
    });
    fireEvent.change(screen.getByLabelText("Source viewer’s Plex token"), {
      target: { value: "fixture-token" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Preview import" }));
    const review = await screen.findByRole("region", { name: "Review import" });
    expect(within(review).getByText(/A remembered film/)).toBeInTheDocument();
    expect(
      within(review).getByRole("button", { name: "Import reviewed records" }),
    ).toBeInTheDocument();
    expect(calls.some((c) => c.url.endsWith("/migration/import"))).toBe(false);
    expect(
      JSON.parse(String(calls.find((c) => c.url.endsWith("/migration/preview"))?.init?.body))
        .viewers[0].localUserId,
    ).toBe(12);
    expect(screen.getByLabelText("Source viewer’s Plex token")).toHaveValue("");
  });

  it("chooses a CSV viewer independently of the Plex import", async () => {
    backend();
    render(<BackupsAndImport tab="import" />);
    await choose("CSV viewer", "Alex");
    expect(screen.getByRole("button", { name: "CSV viewer" })).toHaveTextContent("Alex");
    expect(screen.getByRole("button", { name: "Ether viewer" })).toHaveTextContent("Sam");
    expect(screen.getByRole("button", { name: /Export CSV/ })).toBeEnabled();
  });

  it("edits the backup schedule in place and saves it from the save bar", async () => {
    const calls = backend();
    render(<BackupsAndImport />);
    expect(screen.queryByRole("button", { name: /Edit backup schedule/ })).toBeNull();
    const every = await screen.findByLabelText("Back up every");
    expect(every).toHaveValue(24);
    expect(await screen.findByText(/ether-2026-10-18|2026/)).toBeInTheDocument();
    fireEvent.change(every, { target: { value: "12" } });
    fireEvent.click(await screen.findByRole("button", { name: /Save changes/ }));
    await waitFor(() =>
      expect(calls.some((c) => c.url.endsWith("/adoption/backups/settings"))).toBe(true),
    );
    const save = calls.find((c) => c.url.endsWith("/adoption/backups/settings"))!;
    expect(save.init?.method).toBe("POST");
    expect(JSON.parse(String(save.init?.body))).toEqual({ intervalHours: 12, retain: 7 });
  });

  it("keeps snapshots and imports on separate tabs", async () => {
    backend();
    const { rerender } = render(<BackupsAndImport />);
    expect(await screen.findByLabelText("Back up every")).toBeVisible();
    expect(screen.queryByRole("heading", { name: "Import from Plex" })).toBeNull();
    fireEvent.change(screen.getByLabelText("Plex server URL"), {
      target: { value: "http://half-typed:32400" },
    });
    rerender(<BackupsAndImport tab="import" />);
    expect(screen.getByRole("heading", { name: "Import from Plex" })).toBeVisible();
    expect(screen.getByLabelText("Back up every")).not.toBeVisible();
    // Switching tabs keeps what was typed.
    expect(screen.getByLabelText("Plex server URL")).toHaveValue("http://half-typed:32400");
  });

  it("downloads snapshots only encrypted with a password it never stores", async () => {
    const calls = backend();
    const { createObjectURL, revokeObjectURL } = URL;
    URL.createObjectURL = () => "blob:x";
    URL.revokeObjectURL = () => {};
    setToken("ether-admin-export");
    const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => {});
    const setItem = vi.spyOn(Storage.prototype, "setItem");
    render(<BackupsAndImport />);
    const download = await screen.findByRole("button", {
      name: "Download ether-2026-10-18.tar.gz",
    });
    expect(download).toBeEnabled();
    fireEvent.click(download);
    expect(screen.getByLabelText("Backup password")).toHaveFocus();
    expect(screen.getByRole("alert")).toHaveTextContent("At least 12 characters");
    expect(screen.getByRole("alert")).toHaveTextContent("restoring one asks for it again");
    expect(calls.some((c) => c.url.includes("/backups/export/"))).toBe(false);
    fireEvent.change(screen.getByLabelText("Backup password"), { target: { value: "too short" } });
    fireEvent.click(download);
    expect(calls.some((c) => c.url.includes("/backups/export/"))).toBe(false);
    fireEvent.change(screen.getByLabelText("Backup password"), {
      target: { value: "correct horse battery" },
    });
    expect(download).toBeEnabled();
    fireEvent.click(download);
    await waitFor(() => expect(calls.some((c) => c.url.includes("/backups/export/"))).toBe(true));
    const exp = calls.find((c) => c.url.includes("/backups/export/"))!;
    expect(exp.init?.method).toBe("POST");
    const expHeaders = exp.init?.headers as Record<string, string>;
    expect(expHeaders["X-Ether-Admin-Token"]).toBe("ether-admin-export");
    expect(expHeaders["X-Plex-Token"]).toBeUndefined();
    expect(JSON.parse(String(exp.init?.body))).toEqual({ password: "correct horse battery" });
    await waitFor(() => expect(click).toHaveBeenCalled());
    expect(setItem.mock.calls.some((c) => String(c[1]).includes("correct horse"))).toBe(false);
    setItem.mockRestore();
    click.mockRestore();
    setToken("");
    Object.assign(URL, { createObjectURL, revokeObjectURL });
  });

  it("restores an encrypted download with its password", async () => {
    setToken("ether-admin-backup");
    const calls = backend();
    render(<BackupsAndImport />);
    fireEvent.change(await screen.findByLabelText("Backup password"), {
      target: { value: "pässword horse battery" },
    });
    const file = new File(["ETHERBK1rest-of-file"], "ether-1.etherbackup");
    fireEvent.change(screen.getByLabelText("Choose backup bundle"), { target: { files: [file] } });
    await waitFor(() => expect(calls.some((c) => c.url.endsWith("/backups/restore"))).toBe(true));
    const headers = calls.find((c) => c.url.endsWith("/backups/restore"))!.init!.headers as Record<
      string,
      string
    >;
    expect(headers["Content-Type"]).toBe("application/octet-stream");
    expect(headers["X-Ether-Request"]).toBe("1");
    // The upload authenticates with the console session, never a Plex token.
    expect(headers["X-Ether-Admin-Token"]).toBe("ether-admin-backup");
    expect(headers["X-Plex-Token"]).toBeUndefined();
    setToken("");
    const sent = Uint8Array.from(atob(headers["X-Ether-Backup-Password"] ?? ""), (c) =>
      c.charCodeAt(0),
    );
    expect(new TextDecoder().decode(sent)).toBe("pässword horse battery");
    expect(headers["X-Ether-Backup-Legacy"]).toBeUndefined();
  });

  it("asks before restoring an unencrypted bundle from an older version", async () => {
    const calls = backend();
    render(
      <>
        <BackupsAndImport />
        <ConfirmRoot />
      </>,
    );
    const file = new File(["\x1f\x8b-old-bundle"], "ether-1.tar.gz");
    fireEvent.change(await screen.findByLabelText("Choose backup bundle"), {
      target: { files: [file] },
    });
    fireEvent.click(await screen.findByRole("button", { name: "Restore" }));
    await waitFor(() => expect(calls.some((c) => c.url.endsWith("/backups/restore"))).toBe(true));
    const headers = calls.find((c) => c.url.endsWith("/backups/restore"))!.init!.headers as Record<
      string,
      string
    >;
    expect(headers["X-Ether-Backup-Legacy"]).toBe("plaintext");
    expect(headers["X-Ether-Backup-Password"]).toBeUndefined();
  });

  it("has no local library tools", async () => {
    const calls = backend();
    render(<BackupsAndImport />);
    await screen.findByLabelText("Back up every");
    expect(screen.queryByText(/Local libraries/i)).toBeNull();
    expect(screen.queryByRole("button", { name: /Add folder|Scan now|Match file/ })).toBeNull();
    expect(calls.some((c) => c.url.includes("/libraries"))).toBe(false);
  });
});
