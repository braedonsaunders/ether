import { fireEvent, screen, waitFor } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { calls, renderApp } from "./harness";

const WAIT = { timeout: 8000 };

describe("Apps Plex placement", () => {
  it("keeps account, remote access, and secure connections on the Plex tab", async () => {
    const { fake } = renderApp("/apps/plex");
    expect(await screen.findByLabelText("Allow remote access", {}, WAIT)).toBeInTheDocument();
    expect(screen.getByText("127.0.0.1:32400")).toBeInTheDocument();
    expect(screen.getByLabelText("Secure connections")).toBeInTheDocument();
    expect(screen.getByLabelText("Owner token")).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Connection checks" })).toBeInTheDocument();
    expect(screen.queryByRole("tab", { name: "Remote access" })).not.toBeInTheDocument();
    expect(screen.queryByRole("tab", { name: "Secure connections" })).not.toBeInTheDocument();
    expect(calls(fake, "POST", "/plex/claim")).toHaveLength(0);
    fireEvent.click(screen.getByRole("tab", { name: "Overview" }));
    await waitFor(() =>
      expect(screen.queryByLabelText("Allow remote access")).not.toBeInTheDocument(),
    );
    expect(screen.queryByRole("spinbutton", { name: "Port" })).not.toBeInTheDocument();
    expect(screen.queryByText("127.0.0.1:32400")).not.toBeInTheDocument();
    expect(screen.queryByText(/LAN:/)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Connect a device" })).not.toBeInTheDocument();
    expect(screen.getByRole("switch", { name: "Plex enabled" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Settings" })).toHaveAttribute("href", "/apps/plex");
  });

  it("drops a disabled Plex route to the overview", async () => {
    renderApp("/apps/secure", {}, (fake) => {
      fake.state.config.frontends = { plex: { enabled: false } };
    });
    expect(await screen.findByRole("heading", { name: "Apps" }, WAIT)).toBeInTheDocument();
    await waitFor(() =>
      expect(screen.queryByRole("tab", { name: "Plex" })).not.toBeInTheDocument(),
    );
    expect(screen.queryByLabelText("Secure connections")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Allow remote access")).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Settings" })).not.toBeInTheDocument();
    expect(document.querySelector('[data-app-id="plex"]')).toHaveTextContent("Off");
  });

  it("does not treat /apps/secure as the Plex page", async () => {
    renderApp("/apps/secure");
    expect(await screen.findByRole("heading", { name: "Apps" }, WAIT)).toBeInTheDocument();
    expect(screen.queryByLabelText("Secure connections")).not.toBeInTheDocument();
    expect(screen.queryByRole("tab", { name: "Plex", selected: true })).not.toBeInTheDocument();
  });
});
