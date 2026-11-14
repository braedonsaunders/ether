import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, useLocation } from "react-router-dom";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import type { ReactNode } from "react";
import Studio from "./Studio";
import { apiFetch, apiStream } from "../../lib/api";
import { readAgentStream } from "./LiveStage";
import { toast } from "@braedonsaunders/appkit-ui";
vi.mock("../../lib/api", () => ({ apiFetch: vi.fn(), apiStream: vi.fn(), errorMessage: String }));
vi.mock("../../lib/extend", () => ({ invalidateRegistry: vi.fn() }));
vi.mock("../../components/agent/agentApi", () => ({
  fetchAgentStatus: async () => ({ enabled: true }),
  fetchAgentMessages: async () => ({ messages: [] }),
  decideAgentApproval: vi.fn(),
}));
vi.mock("@braedonsaunders/appkit-ai/react", () => ({ AgentPanel: () => <div>Conversation</div> }));
vi.mock("../../components/workspace", () => ({
  Workspace: ({ children, actions }: { children: ReactNode; actions: ReactNode }) => (
    <div>
      {actions}
      {children}
    </div>
  ),
}));
vi.mock("./code/CodeMirrorPane", () => ({ default: () => <div>Starter code</div> }));
vi.mock("./CodeStudio", () => ({
  CodeStudio: () => <div>Code editor</div>,
  Reference: () => null,
  ReferenceButton: () => null,
}));
vi.mock("./PermissionSheet", () => ({
  PermissionSheet: () => null,
  RuntimeLimitsCard: () => null,
}));
vi.mock("./StudioActions", () => ({
  StudioActions: ({ onInstall }: { onInstall: () => void }) => (
    <button onClick={onInstall}>Install draft</button>
  ),
}));
vi.mock("./LiveStage", () => ({
  readAgentStream: vi.fn(),
  momentsForTool: async () => [],
  NativePreview: () => null,
  LiveStageView: ({ moments }: { moments: { text?: string }[] }) => (
    <div>
      {moments.map((m, i) => (
        <p key={i}>{m.text}</p>
      ))}
    </div>
  ),
}));
function Location() {
  const l = useLocation();
  return (
    <output data-testid="location">
      {l.pathname}
      {l.search}
    </output>
  );
}
function mount(path = "/plugins/studio") {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <Studio />
      <Location />
    </MemoryRouter>,
  );
}
function send(text: string) {
  const input = screen.getByRole("textbox");
  fireEvent.change(input, { target: { value: text } });
  fireEvent.keyDown(input, { key: "Enter" });
}
const project = {
  id: "viktor",
  name: "Viktor",
  template: "list",
  files: { "src/plugin.ts": "" },
  diagnostics: [],
  updatedAt: "2026-11-03T12:00:00Z",
};
beforeEach(() => {
  sessionStorage.clear();
  vi.clearAllMocks();
  vi.mocked(apiFetch).mockResolvedValue({ projects: [] });
  vi.mocked(apiStream).mockResolvedValue(
    new Response("", { headers: { "X-Ether-Thread-Id": "thread-one" } }),
  );
});
afterEach(cleanup);
it("serializes queued followups in the existing thread", async () => {
  let finish!: () => void;
  vi.mocked(readAgentStream)
    .mockImplementationOnce(
      () =>
        new Promise<void>((r) => {
          finish = r;
        }),
    )
    .mockResolvedValue(undefined);
  mount();
  send("First request");
  await waitFor(() => expect(readAgentStream).toHaveBeenCalledTimes(1));
  send("More detail");
  expect(apiStream).toHaveBeenCalledTimes(1);
  expect(screen.getByText("Your follow-up will run after this step finishes.")).toBeInTheDocument();
  await act(async () => finish());
  await waitFor(() => expect(apiStream).toHaveBeenCalledTimes(2));
  expect(JSON.parse(vi.mocked(apiStream).mock.calls[1]![1]!.body as string)).toMatchObject({
    threadId: "thread-one",
    prompt: "More detail",
  });
  expect(await screen.findByText("Conversation")).toBeInTheDocument();
});
it("keeps stream failures visible and does not claim the draft is ready", async () => {
  vi.mocked(readAgentStream).mockImplementation(async (_r, onChunk) =>
    onChunk({ type: "error", errorText: "Model unavailable" }),
  );
  mount();
  send("Build this");
  expect(await screen.findByRole("alert")).toHaveTextContent("Model unavailable");
  expect(screen.queryByText("Your draft is ready")).toBeNull();
  expect(screen.getByRole("button", { name: "Open draft" })).toBeInTheDocument();
});
it("stop aborts the active turn and never sends queued followups", async () => {
  let finish!: () => void;
  vi.mocked(readAgentStream).mockImplementation(
    () =>
      new Promise<void>((r) => {
        finish = r;
      }),
  );
  mount();
  send("First");
  await waitFor(() => expect(readAgentStream).toHaveBeenCalledTimes(1));
  send("Second");
  fireEvent.click(screen.getByRole("button", { name: "Stop" }));
  expect(vi.mocked(apiStream).mock.calls[0]![1]!.signal!.aborted).toBe(true);
  await act(async () => finish());
  expect(apiStream).toHaveBeenCalledTimes(1);
});
it("persists an AI draft URL and uses the server install record for navigation and toast", async () => {
  sessionStorage.setItem("ether-studio-thread", "thread-one");
  vi.mocked(apiFetch).mockImplementation(async (path) => {
    if (path === "/plugins/studio/projects") return { projects: [project] };
    if (path.endsWith("install/preview"))
      return { preview: { manifest: { name: "Viktor" }, permissions: [] } };
    if (path.endsWith("/install"))
      return { plugin: { ID: "viktor-installed", Name: "Viktor Installed" } };
    return { project };
  });
  const success = vi.spyOn(toast, "success");
  mount();
  await waitFor(() => expect(screen.getByTestId("location")).toHaveTextContent("project=viktor"));
  fireEvent.click(await screen.findByRole("button", { name: "Install draft" }));
  fireEvent.click(await screen.findByRole("button", { name: /^Install$/ }));
  await waitFor(() =>
    expect(screen.getByTestId("location")).toHaveTextContent(
      "/plugins/installed?plugin=viktor-installed",
    ),
  );
  expect(success).toHaveBeenCalledWith("Viktor Installed is installed");
  success.mockRestore();
});

it("offers saved drafts from Code home and reopens the chosen URL", async () => {
  const other = { ...project, id: "priya", name: "Priya", updatedAt: "2026-11-04T12:00:00Z" };
  vi.mocked(apiFetch).mockImplementation(async (path) => {
    if (path.endsWith("/templates")) return { templates: [] };
    if (path === "/plugins/studio/projects") return { projects: [other, project] };
    return { project: path.endsWith("/priya") ? other : project };
  });
  mount("/plugins/studio?view=code");
  expect(await screen.findByRole("region", { name: "Saved drafts" })).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Viktor" }));
  await waitFor(() =>
    expect(screen.getByTestId("location")).toHaveTextContent("view=code&project=viktor"),
  );
  expect(await screen.findByText("Code editor")).toBeInTheDocument();
  expect(apiFetch).not.toHaveBeenCalledWith("/plugins/studio/projects/priya");
});

it("opens the draft created by this conversation even when another draft is newer", async () => {
  vi.mocked(readAgentStream).mockImplementation(async (_r, onChunk) => {
    onChunk({
      type: "tool-input-available",
      toolCallId: "create",
      toolName: "studio_project_create",
    });
    onChunk({ type: "tool-output-available", toolCallId: "create", output: { project } });
  });
  vi.mocked(apiFetch).mockImplementation(async (path) => {
    if (path === "/plugins/studio/projects")
      return {
        projects: [{ ...project, id: "someone-else", updatedAt: "2026-11-04T12:00:00Z" }, project],
      };
    return { project };
  });
  mount();
  send("Create my draft");
  await waitFor(() => expect(screen.getByTestId("location")).toHaveTextContent("project=viktor"));
  expect(apiFetch).not.toHaveBeenCalledWith("/plugins/studio/projects/someone-else");
});
