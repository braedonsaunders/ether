import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { calls, renderApp } from "./harness";

const WAIT = { timeout: 4000 };

async function openActions() {
  fireEvent.click(await screen.findByRole("button", { name: /Actions/ }, WAIT));
  return screen.findByRole("menuitem", { name: "Rebuild rows" }, WAIT);
}

async function pick(name: string) {
  fireEvent.click(await screen.findByRole("menuitem", { name }, WAIT));
}

describe("Viewer Actions menu", () => {
  it("rebuilds rows from the menu", async () => {
    const { fake } = renderApp("/viewers/2/overview");
    await openActions();
    fireEvent.click(screen.getByRole("menuitem", { name: "Rebuild rows" }));
    await waitFor(() => expect(calls(fake, "POST", "/viewers/2/refresh")).toHaveLength(1), WAIT);
  });

  it("pauses with an explanation, then resumes", async () => {
    const { fake } = renderApp("/viewers/2/overview");
    await openActions();
    await pick("Pause access");
    // The confirm explains that everything is kept.
    expect(await screen.findByText(/keeps everything/, {}, WAIT)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Pause access" }));
    await waitFor(() => expect(calls(fake, "POST", "/users/2/pause")).toHaveLength(1), WAIT);
    expect(await screen.findByText(/Access is paused/, {}, WAIT)).toBeInTheDocument();

    await openActions();
    await pick("Resume access");
    await waitFor(() => expect(calls(fake, "POST", "/users/2/resume")).toHaveLength(1), WAIT);
    await waitFor(() => expect(screen.queryByText(/Access is paused/)).toBeNull(), WAIT);
  });

  it("signs out everywhere after confirming", async () => {
    const { fake } = renderApp("/viewers/2/overview");
    await openActions();
    await pick("Sign out everywhere");
    fireEvent.click(await screen.findByRole("button", { name: "Sign out everywhere" }, WAIT));
    await waitFor(() => expect(calls(fake, "POST", "/users/2/signout")).toHaveLength(1), WAIT);
  });

  it("shows an invite link with a copy button", async () => {
    const { fake } = renderApp("/viewers/2/overview");
    await openActions();
    await pick("Send invite link");
    expect(await screen.findByText(/invite=mock-secret/, {}, WAIT)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Copy link/ })).toBeInTheDocument();
    expect(calls(fake, "POST", "/users/2/invite")).toHaveLength(1);
  });

  it("approves a 6-character TV code for the viewer", async () => {
    const { fake } = renderApp("/viewers/2/overview");
    await openActions();
    await pick("Approve a TV code");
    fireEvent.change(await screen.findByLabelText("TV code", {}, WAIT), {
      target: { value: "abc234" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Approve" }));
    await waitFor(
      () => expect(calls(fake, "POST", "/viewers/2/quickconnect")).toHaveLength(1),
      WAIT,
    );
    expect(calls(fake, "POST", "/viewers/2/quickconnect")[0]!.body).toEqual({ code: "ABC234" });
  });

  it("resets the password through the drawer", async () => {
    const { fake } = renderApp("/viewers/2/overview");
    await openActions();
    await pick("Reset password");
    fireEvent.change(await screen.findByLabelText("New password", {}, WAIT), {
      target: { value: "a-long-enough-password" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Set or reset password" }));
    await waitFor(() => expect(calls(fake, "POST", "/users/2/password")).toHaveLength(1), WAIT);
    expect(calls(fake, "POST", "/users/2/password")[0]!.body).toEqual({
      username: "sam",
      password: "a-long-enough-password",
    });
  });

  it("merges into the picked viewer and opens their page", async () => {
    const { fake } = renderApp("/viewers/2/overview");
    await openActions();
    await pick("Merge into another viewer");
    const dialog = await screen.findByRole("dialog", { name: "Merge into another viewer" }, WAIT);
    expect(within(dialog).getByText(/Watch history, resume points/)).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("radio", { name: /The Kids/ }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Merge" }));
    const confirm = await screen.findByRole(
      "dialog",
      { name: /Merge Sam Rivera into The Kids/ },
      WAIT,
    );
    fireEvent.click(within(confirm).getByRole("button", { name: "Merge" }));
    await waitFor(() => expect(calls(fake, "POST", "/users/2/merge")).toHaveLength(1), WAIT);
    expect(calls(fake, "POST", "/users/2/merge")[0]!.body).toEqual({ into: 3 });
    expect(await screen.findByRole("heading", { name: /The Kids/ }, WAIT)).toBeInTheDocument();
  });

  it("removes only after the name is typed", async () => {
    const { fake } = renderApp("/viewers/2/overview");
    await openActions();
    await pick("Remove viewer");
    const dialog = await screen.findByRole("dialog", { name: "Remove viewer" }, WAIT);
    const submit = within(dialog).getByRole("button", { name: "Remove viewer" });
    expect(submit).toBeDisabled();
    fireEvent.change(within(dialog).getByLabelText("Type their name to confirm"), {
      target: { value: "Sam Rivera" },
    });
    expect(submit).toBeEnabled();
    fireEvent.click(submit);
    await waitFor(() => expect(calls(fake, "DELETE", "/users/2")).toHaveLength(1), WAIT);
    // Back on the list, without Sam.
    expect(await screen.findByText("The Kids", {}, WAIT)).toBeInTheDocument();
    expect(screen.queryByText("Sam Rivera")).toBeNull();
  });

  it("hides pause, remove, merge and invite on the owner's page", async () => {
    renderApp("/viewers/1/overview");
    await openActions();
    expect(screen.queryByRole("menuitem", { name: "Pause access" })).toBeNull();
    expect(screen.queryByRole("menuitem", { name: "Remove viewer" })).toBeNull();
    expect(screen.queryByRole("menuitem", { name: "Merge into another viewer" })).toBeNull();
    expect(screen.queryByRole("menuitem", { name: "Send invite link" })).toBeNull();
    expect(screen.getByRole("menuitem", { name: "Reset password" })).toBeInTheDocument();
    expect(screen.getByRole("menuitem", { name: "Approve a TV code" })).toBeInTheDocument();
  });

  it("sends Plex Home viewers to plex.tv instead of managing them", async () => {
    const open = vi.spyOn(window, "open").mockImplementation(() => null);
    renderApp("/viewers/3/overview");
    expect(
      await screen.findByText(
        "Managed on plex.tv. Access changes happen there, not in Ether.",
        {},
        WAIT,
      ),
    ).toBeInTheDocument();
    await openActions();
    expect(screen.queryByRole("menuitem", { name: "Pause access" })).toBeNull();
    expect(screen.queryByRole("menuitem", { name: "Remove viewer" })).toBeNull();
    expect(screen.queryByRole("menuitem", { name: "Merge into another viewer" })).toBeNull();
    expect(screen.queryByRole("menuitem", { name: "Send invite link" })).toBeNull();
    expect(screen.queryByRole("menuitem", { name: "Reset password" })).toBeNull();
    expect(screen.getByRole("menuitem", { name: "Approve a TV code" })).toBeInTheDocument();
    expect(screen.getByRole("menuitem", { name: "Sign out everywhere" })).toBeInTheDocument();
    fireEvent.click(screen.getByRole("menuitem", { name: "Manage on plex.tv" }));
    expect(open).toHaveBeenCalledWith(
      "https://app.plex.tv/desktop/#!/settings/manage-library-access",
      "_blank",
      "noopener,noreferrer",
    );
    open.mockRestore();
  });
});

describe("Max quality", () => {
  it("writes a per-viewer quality rule and Auto removes it", async () => {
    const { fake } = renderApp("/viewers/2/limits");
    const group = await screen.findByRole("radiogroup", { name: "Max quality" }, WAIT);
    // Only the levels with a matching profile are offered (no 720p profile).
    expect(within(group).getByRole("radio", { name: "Auto" })).toBeInTheDocument();
    expect(within(group).getByRole("radio", { name: "4K" })).toBeInTheDocument();
    expect(within(group).queryByRole("radio", { name: "720p" })).toBeNull();

    fireEvent.click(within(group).getByRole("radio", { name: "1080p" }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/quality")).toHaveLength(1), WAIT);
    const written = calls(fake, "PUT", "/config/quality")[0]!.body as {
      rules: { userId?: number; profile: string }[];
    };
    expect(written.rules[0]).toEqual({ userId: 2, profile: "1080p" });
    await waitFor(
      () =>
        expect(within(group).getByRole("radio", { name: "1080p" })).toHaveAttribute(
          "aria-checked",
          "true",
        ),
      WAIT,
    );

    fireEvent.click(within(group).getByRole("radio", { name: "Auto" }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/quality")).toHaveLength(2), WAIT);
    const cleared = calls(fake, "PUT", "/config/quality")[1]!.body as {
      rules: { userId?: number; profile: string }[];
    };
    expect(cleared.rules.some((r) => r.userId === 2)).toBe(false);
  });
});
