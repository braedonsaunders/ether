import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { SimklIntegration } from "../pages/settings/SimklIntegration";

const { fetchMock, users, putSection } = vi.hoisted(() => ({
  fetchMock: vi.fn(),
  users: vi.fn(),
  putSection: vi.fn(),
}));
vi.mock("../lib/api", () => ({ apiFetch: fetchMock, api: { users, putSection } }));
describe("Simkl integration", () => {
  beforeEach(() => {
    vi.resetAllMocks();
    users.mockResolvedValue([
      { id: 1, title: "Alice" },
      { id: 2, title: "Bob" },
    ]);
    fetchMock.mockImplementation(async (path: string) => {
      if (path === "/config/simkl")
        return { value: { enabled: true, clientId: "id", clientSecret: "••••" } };
      if (path.endsWith("/status")) return { connected: false };
      if (path.endsWith("/start"))
        return {
          userCode: "ABCD-EFGH",
          verificationUri: "https://simkl.com/pin",
          verificationUriComplete: "https://simkl.com/pin?user_code=ABCD-EFGH",
          expiresAt: "2099-01-01T00:00:00Z",
          interval: 5,
        };
      if (path.endsWith("/poll")) return { status: "authorized" };
      return {};
    });
  });
  it("binds connection and polling to the selected viewer", async () => {
    render(<SimklIntegration />);
    await screen.findByLabelText("Viewer");
    fireEvent.change(screen.getByLabelText("Viewer"), { target: { value: "2" } });
    fireEvent.click(screen.getByRole("button", { name: "Connect" }));
    expect(await screen.findByText("ABCD-EFGH")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Check connection" }));
    await waitFor(() =>
      expect(fetchMock).toHaveBeenCalledWith("/simkl/2/poll", { method: "POST" }),
    );
    expect(fetchMock).toHaveBeenCalledWith("/simkl/2/start", { method: "POST" });
    expect(fetchMock).not.toHaveBeenCalledWith("/simkl/1/poll", { method: "POST" });
  });
});
