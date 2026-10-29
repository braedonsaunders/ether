import { render, screen, fireEvent, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { describe, it, expect, vi, afterEach } from "vitest";
import { webcrypto } from "node:crypto";
import { BrowserPush } from "../pages/notifications/BrowserPush";
import { ChannelDrawer } from "../pages/notifications/ChannelDrawer";
import { Channels } from "../pages/notifications/Channels";
import { Protection } from "../pages/notifications/Protection";
import { blankChannel, type NotificationsData } from "../pages/notifications/types";

const empty: NotificationsData = {
  channels: [],
  events: ["server.crash", "playback.error", "newsletter.weekly"],
  deliveries: [],
};
afterEach(() => vi.unstubAllGlobals());
function pushSupport(permission = "granted") {
  const requestPermission = vi.fn().mockResolvedValue(permission);
  vi.stubGlobal("isSecureContext", true);
  vi.stubGlobal("Notification", { permission: "default", requestPermission });
  vi.stubGlobal("PushManager", class {});
  vi.stubGlobal("crypto", webcrypto);
  const subscription = {
    endpoint: "https://push.example.test/device",
    toJSON: () => ({
      endpoint: "https://push.example.test/device",
      keys: { auth: "auth", p256dh: "key" },
    }),
    unsubscribe: vi.fn().mockResolvedValue(true),
  };
  const subscribe = vi.fn().mockResolvedValue(subscription);
  vi.stubGlobal("navigator", {
    platform: "Test phone",
    serviceWorker: {
      register: vi.fn().mockResolvedValue({}),
      ready: Promise.resolve({
        pushManager: { getSubscription: vi.fn().mockResolvedValue(null), subscribe },
      }),
    },
  });
  return { requestPermission, subscription, subscribe };
}
describe("Notification setup", () => {
  it("opens, saves and edits a channel on an HTTP LAN page without randomUUID", async () => {
    // randomUUID is secure-context-only; getRandomValues is available on HTTP.
    vi.stubGlobal("isSecureContext", false);
    vi.stubGlobal("crypto", { getRandomValues: webcrypto.getRandomValues.bind(webcrypto) });
    const fetcher = vi.fn().mockResolvedValue(new Response("{}", { status: 200 }));
    vi.stubGlobal("fetch", fetcher);
    const onChanged = vi.fn().mockResolvedValue(undefined);
    const { rerender } = render(<Channels data={empty} onChanged={onChanged} />);
    fireEvent.click(screen.getByRole("button", { name: "New channel" }));
    const drawer = await screen.findByRole("dialog", { name: "New notification channel" });
    fireEvent.change(within(drawer).getByLabelText("Name"), { target: { value: "Server alerts" } });
    fireEvent.change(within(drawer).getByLabelText("Endpoint URL"), {
      target: { value: "https://notify.example.test/hook" },
    });
    fireEvent.click(within(drawer).getByRole("button", { name: "Save channel" }));
    await waitFor(() => expect(onChanged).toHaveBeenCalledOnce());
    const [url, request] = fetcher.mock.calls[0]!;
    const saved = JSON.parse(request.body);
    expect(saved.id).toEqual(expect.any(String));
    expect(saved.id.length).toBeGreaterThan(0);
    expect(url).toContain(`/notifications/${saved.id}`);
    expect(request.method).toBe("PUT");
    expect(saved).toMatchObject({
      name: "Server alerts",
      endpoint: "https://notify.example.test/hook",
    });
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    rerender(<Channels data={{ ...empty, channels: [saved] }} onChanged={onChanged} />);
    fireEvent.click(screen.getByRole("button", { name: "Edit" }));
    const editor = await screen.findByRole("dialog", { name: "Edit notification channel" });
    expect(within(editor).getByLabelText("Name")).toHaveValue("Server alerts");
    fireEvent.click(within(editor).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(fetcher).toHaveBeenCalledOnce();
  });

  it("saves all email fields, recipient lists and explicit TLS mode", () => {
    const onSave = vi.fn();
    render(
      <ChannelDrawer
        open
        channel={{ ...blankChannel(), kind: "smtp" }}
        isNew
        events={empty.events}
        saving={false}
        error={null}
        onClose={() => {}}
        onSave={onSave}
      />,
    );
    const drawer = screen.getByRole("dialog");
    fireEvent.change(within(drawer).getByLabelText("SMTP host:port"), {
      target: { value: "smtp.example.test:587" },
    });
    fireEvent.change(within(drawer).getByLabelText("From address"), {
      target: { value: "owner@example.test" },
    });
    fireEvent.change(within(drawer).getByLabelText("To addresses"), {
      target: { value: "one@example.test, two@example.test" },
    });
    fireEvent.click(within(drawer).getByLabelText("Weekly newsletter"));
    fireEvent.click(within(drawer).getByRole("button", { name: "Save channel" }));
    expect(onSave).toHaveBeenCalledWith(
      expect.objectContaining({
        smtpMode: "starttls",
        endpoint: "smtp.example.test:587",
        to: "one@example.test, two@example.test",
        events: expect.arrayContaining(["server.crash", "newsletter.weekly"]),
      }),
    );
  });
  it("tests a paused channel directly and reloads failed delivery history", async () => {
    const onChanged = vi.fn().mockResolvedValue(undefined);
    const fetcher = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ error: { message: "SMTP authentication failed" } }), {
        status: 502,
      }),
    );
    vi.stubGlobal("fetch", fetcher);
    render(
      <Channels
        data={{
          ...empty,
          channels: [{ ...blankChannel(), id: "mail", kind: "smtp", enabled: false }],
        }}
        onChanged={onChanged}
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: "Send test" }));
    await waitFor(() => expect(onChanged).toHaveBeenCalled());
    expect(fetcher).toHaveBeenCalledWith(
      expect.stringContaining("/notifications/mail/test"),
      expect.objectContaining({ method: "POST" }),
    );
  });
  it("does not subscribe or save when browser permission is declined", async () => {
    const { subscribe } = pushSupport("denied");
    const fetcher = vi.fn();
    vi.stubGlobal("fetch", fetcher);
    render(<BrowserPush data={empty} onChanged={vi.fn()} />);
    fireEvent.click(screen.getByRole("button", { name: "Connect this device" }));
    expect(await screen.findByRole("status")).toHaveTextContent("Allow notifications");
    expect(subscribe).not.toHaveBeenCalled();
    expect(fetcher).not.toHaveBeenCalled();
  });
  it("registers a device and saves its encrypted push subscription", async () => {
    const { subscribe } = pushSupport();
    const onChanged = vi.fn().mockResolvedValue(undefined);
    const fetcher = vi.fn(
      async (url: string) =>
        new Response(
          JSON.stringify(url.endsWith("/push/key") ? { publicKey: btoa("test-key") } : {}),
          { status: 200 },
        ),
    );
    vi.stubGlobal("fetch", fetcher);
    render(<BrowserPush data={empty} onChanged={onChanged} />);
    fireEvent.click(screen.getByRole("button", { name: "Connect this device" }));
    await waitFor(() => expect(onChanged).toHaveBeenCalled());
    expect(subscribe).toHaveBeenCalledWith(expect.objectContaining({ userVisibleOnly: true }));
    const put = fetcher.mock.calls.find(
      (c) => (c as unknown as [string, RequestInit])[1]?.method === "PUT",
    ) as unknown as [string, RequestInit];
    expect(JSON.parse(String(put[1].body))).toMatchObject({
      kind: "webpush",
      subscription: { endpoint: "https://push.example.test/device" },
      events: expect.arrayContaining(["server.crash"]),
    });
  });
  it("removes a newly created subscription if saving the destination fails", async () => {
    const { subscription } = pushSupport();
    vi.stubGlobal(
      "fetch",
      vi.fn(
        async (url: string) =>
          new Response(
            JSON.stringify(
              url.endsWith("/push/key")
                ? { publicKey: btoa("key") }
                : { error: { message: "Save failed" } },
            ),
            { status: url.endsWith("/push/key") ? 200 : 500 },
          ),
      ),
    );
    render(<BrowserPush data={empty} onChanged={vi.fn()} />);
    fireEvent.click(screen.getByRole("button", { name: "Connect this device" }));
    expect(await screen.findByRole("status")).toHaveTextContent("Save failed");
    expect(subscription.unsubscribe).toHaveBeenCalled();
  });
  it("saves external outage monitoring without exposing its existing URL", async () => {
    const fetcher = vi.fn(
      async (_url: string, init?: RequestInit) =>
        new Response(
          JSON.stringify(
            init?.method ? { ok: true } : { enabled: false, endpoint: "[configured]" },
          ),
          { status: 200 },
        ),
    );
    vi.stubGlobal("fetch", fetcher);
    render(
      <MemoryRouter>
        <Protection active />
      </MemoryRouter>,
    );
    fireEvent.click(await screen.findByRole("switch", { name: "Send uptime heartbeat" }));
    fireEvent.click(screen.getByRole("button", { name: "Save heartbeat" }));
    await waitFor(() =>
      expect(
        fetcher.mock.calls.some(
          (c) =>
            c[1]?.method === "PUT" && JSON.parse(String(c[1].body)).endpoint === "[configured]",
        ),
      ).toBe(true),
    );
    expect(screen.getByText("Watcher running on this node")).toBeInTheDocument();
  });
});
