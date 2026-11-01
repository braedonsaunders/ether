import { fireEvent, screen } from "@testing-library/react";
import { expect, it } from "vitest";
import { renderApp } from "./harness";

it("opens the owner's account from the header and keeps invite links off that page", async () => {
  renderApp("/", {}, (fake) => {
    fake.state.config.frontends = {
      ...fake.state.config.frontends,
      jellyfin: { enabled: true },
    };
  });
  fireEvent.click(await screen.findByRole("link", { name: "Your account" }));
  expect(await screen.findByRole("heading", { name: "Your account" })).toBeInTheDocument();
  expect(await screen.findByRole("heading", { name: "Connect apps" })).toBeInTheDocument();
  expect(await screen.findByRole("heading", { name: "Sign-in methods" })).toBeInTheDocument();
  expect(screen.getByLabelText("New password")).toBeInTheDocument();
  expect(screen.getByRole("heading", { name: "Devices" })).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Create invite link" })).not.toBeInTheDocument();
});
