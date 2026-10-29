import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { ViewerPush } from "../pages/me/ViewerPush";

afterEach(() => vi.unstubAllGlobals());

function pushSupport(hasSubscription: boolean) {
  vi.stubGlobal("isSecureContext", true);
  vi.stubGlobal("Notification", { requestPermission: vi.fn().mockResolvedValue("granted") });
  vi.stubGlobal("PushManager", class {});
  const subscription = {
    endpoint: "https://push.example.test/secret",
    toJSON: () => ({
      endpoint: "https://push.example.test/secret",
      keys: { auth: "auth", p256dh: "key" },
    }),
    unsubscribe: vi.fn().mockResolvedValue(true),
  };
  const manager = {
    getSubscription: vi.fn().mockResolvedValue(hasSubscription ? subscription : null),
    subscribe: vi.fn().mockResolvedValue(subscription),
  };
  vi.stubGlobal("navigator", {
    serviceWorker: {
      getRegistration: vi.fn().mockResolvedValue({ pushManager: manager }),
      register: vi.fn().mockResolvedValue({}),
      ready: Promise.resolve({ pushManager: manager }),
    },
  });
  return subscription;
}

it("checks viewer-specific registration and removes only that association", async () => {
  const subscription = pushSupport(true);
  const status = vi.fn().mockResolvedValue({ registered: true });
  const remove = vi.fn().mockResolvedValue(undefined);
  render(<ViewerPush getKey={vi.fn()} status={status} subscribe={vi.fn()} remove={remove} />);
  const disconnect = await screen.findByRole("button", { name: "Disconnect this device" });
  expect(status).toHaveBeenCalledWith(subscription.endpoint);
  fireEvent.click(disconnect);
  await waitFor(() => expect(remove).toHaveBeenCalledWith(subscription.endpoint));
  await screen.findByRole("button", { name: "Connect this device" });
  expect(subscription.unsubscribe).not.toHaveBeenCalled();
});

it("registers this viewer and cleans up a newly created subscription if saving fails", async () => {
  const subscription = pushSupport(false);
  const save = vi.fn().mockRejectedValue(new Error("offline"));
  render(
    <ViewerPush
      getKey={vi.fn().mockResolvedValue({ publicKey: "AQ" })}
      status={vi.fn()}
      subscribe={save}
      remove={vi.fn()}
    />,
  );
  const connect = await screen.findByRole("button", { name: "Connect this device" });
  await waitFor(() => expect(connect).toBeEnabled());
  fireEvent.click(connect);
  await screen.findByRole("alert");
  expect(save).toHaveBeenCalledWith(subscription.toJSON());
  expect(subscription.unsubscribe).toHaveBeenCalledOnce();
});
