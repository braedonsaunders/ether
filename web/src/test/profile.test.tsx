import { fireEvent, screen, waitFor } from "@testing-library/react";
import { expect, it } from "vitest";
import { renderApp } from "./harness";

const WAIT = { timeout: 4000 };

it("opens the owner's own viewer page from the header, with their sign-in tab", async () => {
  renderApp("/", {}, (fake) => {
    fake.state.config.frontends = {
      ...fake.state.config.frontends,
      jellyfin: { enabled: true },
    };
  });
  const account = await screen.findByRole("link", { name: "Your account" }, WAIT);
  // The header link lands on the owner's viewer page, not a separate page.
  await waitFor(() => expect(account).toHaveAttribute("href", "/viewers/1"), WAIT);
  fireEvent.click(account);
  expect(await screen.findByRole("heading", { name: /Alex Rivera/ }, WAIT)).toBeInTheDocument();
  fireEvent.click(await screen.findByRole("tab", { name: "Sign-in & devices" }, WAIT));
  expect(await screen.findByRole("heading", { name: "Connect apps" }, WAIT)).toBeInTheDocument();
  expect(await screen.findByRole("heading", { name: "Sign-in methods" }, WAIT)).toBeInTheDocument();
  // Passwords are reset from the Actions drawer, not an inline form.
  expect(screen.queryByLabelText("New password")).not.toBeInTheDocument();
  expect(screen.getByRole("heading", { name: "Devices" })).toBeInTheDocument();
  // Six sequential waits of up to 4 s each cannot fit vitest's 5 s default.
}, 20_000);

it("has no separate /profile page", async () => {
  renderApp("/profile");
  // Unknown routes land on the overview; there is no Your account page.
  expect(await screen.findByRole("link", { name: "Your account" }, WAIT)).toBeInTheDocument();
  expect(screen.queryByRole("heading", { name: "Your account" })).not.toBeInTheDocument();
  expect(screen.getByRole("link", { name: "Your account" })).not.toHaveAttribute(
    "href",
    "/profile",
  );
});
