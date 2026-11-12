import { fireEvent, screen, waitFor } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { calls, renderApp } from "./harness";

const WAIT = { timeout: 8000 };

describe("Apps Plex placement", () => {
  it("loads and saves the availability check timeout in seconds", async () => {
    const { fake } = renderApp("/apps/plex", {}, (f) => {
      f.state.config.plex.fileCheckTimeout = "17s";
    });
    fireEvent.click(
      await screen.findByRole("button", { name: "More options: Compatibility" }, WAIT),
    );
    const input = screen.getByLabelText("Availability check timeout");
    expect(input).toHaveValue(17);
    expect(
      screen.getByRole("button", { name: "Availability check timeout unit" }),
    ).toHaveTextContent("Seconds");
    fireEvent.change(input, { target: { value: "15" } });
    fireEvent.click(screen.getByRole("button", { name: /Save changes/ }));
    await waitFor(() => expect(fake.state.config.plex.fileCheckTimeout).toBe("15s"));
    expect(calls(fake, "PUT", "/config/plex")[0]?.body).toMatchObject({ fileCheckTimeout: "15s" });
    expect(await screen.findByLabelText("Availability check timeout", {}, WAIT)).toHaveValue(15);
  });

  it("keeps account, remote access, and secure connections on the Plex tab", async () => {
    const { fake } = renderApp("/apps/plex");
    expect(
      await screen.findByRole("button", { name: "Sign out of Plex" }, WAIT),
    ).toBeInTheDocument();
    expect(screen.getByText(/192\.168\.1\.20:32400/)).toBeInTheDocument();
    expect(screen.queryByText(/127\.0\.0\.1/)).not.toBeInTheDocument();
    expect(screen.getAllByText("Claimed by Plex account Braedon").length).toBeGreaterThan(0);
    expect(screen.queryByLabelText("Owner token")).not.toBeInTheDocument();
    expect(screen.queryByRole("switch", { name: "Claimed" })).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("tab", { name: "Remote access" }));
    expect(await screen.findByLabelText("Allow remote access", {}, WAIT)).toBeInTheDocument();
    expect(screen.getByLabelText("Secure connections")).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Connection checks" })).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Advanced" })).not.toBeInTheDocument();
    expect(calls(fake, "POST", "/plex/claim")).toHaveLength(0);
    fireEvent.click(screen.getByRole("tab", { name: "Overview" }));
    await waitFor(() =>
      expect(screen.queryByLabelText("Allow remote access")).not.toBeInTheDocument(),
    );
    expect(screen.queryByRole("spinbutton", { name: "Port" })).not.toBeInTheDocument();
    expect(screen.queryByText(/192\.168\.1\.20:32400/)).not.toBeInTheDocument();
    expect(screen.queryByText(/LAN:/)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Connect a device" })).not.toBeInTheDocument();
    expect(screen.getByRole("switch", { name: "Plex enabled" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Settings" })).toHaveAttribute("href", "/apps/plex");
  });

  it("drops a disabled Plex route to the overview", async () => {
    renderApp("/apps/plex/remote", {}, (fake) => {
      fake.state.config.frontends = { plex: { enabled: false } };
    });
    expect(await screen.findByRole("heading", { name: "Apps" }, WAIT)).toBeInTheDocument();
    await waitFor(() =>
      expect(screen.queryByRole("tab", { name: "Plex" })).not.toBeInTheDocument(),
    );
    expect(screen.queryByLabelText("Secure connections")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Allow remote access")).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Settings" })).not.toBeInTheDocument();
    await waitFor(() =>
      expect(document.querySelector('[data-app-id="plex"]')).toHaveTextContent("Off"),
    );
  });

  it("opens remote access and secure connections at /apps/plex/remote", async () => {
    renderApp("/apps/plex/remote");
    expect(await screen.findByLabelText("Allow remote access", {}, WAIT)).toBeInTheDocument();
    expect(screen.getByLabelText("Secure connections")).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "Remote access", selected: true })).toBeInTheDocument();
  });
});
