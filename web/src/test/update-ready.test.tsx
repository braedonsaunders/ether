import { fireEvent, render, screen } from "@testing-library/react";
import { useState } from "react";
import { expect, it, vi } from "vitest";
import { SaveBar, SaveBarHost } from "../components/SaveBar";
import { UpdateReady } from "../components/UpdateReady";
import { consoleEditing } from "../lib/console-editing";

it("keeps a focused field on screen when the server version changes", async () => {
  let version = "1";
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => new Response(JSON.stringify({ version }))),
  );
  const reload = vi.fn();
  vi.stubGlobal("location", { ...window.location, reload });
  render(
    <>
      <label>
        Code
        <input aria-label="Code" />
      </label>
      <UpdateReady />
    </>,
  );
  await vi.waitFor(() => expect(fetch).toHaveBeenCalled());
  const field = screen.getByLabelText("Code");
  fireEvent.focus(field);
  fireEvent.change(field, { target: { value: "ABC" } });
  expect(consoleEditing()).toBe(true);
  version = "2";
  fireEvent(window, new Event("vite:preloadError", { cancelable: true }));
  expect(await screen.findByText("Ether was updated — reload when ready")).toBeInTheDocument();
  expect(reload).not.toHaveBeenCalled();
  expect(field).toHaveValue("ABC");
  fireEvent.click(screen.getByRole("button", { name: "Reload" }));
  expect(reload).toHaveBeenCalledOnce();
});

it("treats an unsaved save bar as a reason to leave the page alone", async () => {
  function Draft() {
    const [dirty, setDirty] = useState(false);
    return (
      <SaveBarHost>
        <input
          aria-label="Name"
          defaultValue="saved"
          onChange={(e) => setDirty(e.target.value !== "saved")}
        />
        <SaveBar dirty={dirty} onSave={() => undefined} onDiscard={() => setDirty(false)} />
      </SaveBarHost>
    );
  }
  render(<Draft />);
  expect(consoleEditing()).toBe(false);
  fireEvent.change(screen.getByLabelText("Name"), { target: { value: "edited" } });
  await vi.waitFor(() => expect(consoleEditing()).toBe(true));
});
