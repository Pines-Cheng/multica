import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import { setApiInstance } from "@multica/core/api";
import type { ApiClient } from "@multica/core/api/client";
import type { IssueChannelChat } from "@multica/core/types";
import { RESOURCES } from "../../locales";
import { IssueChannelChatButton } from "./issue-channel-chat-button";

const openExternal = vi.hoisted(() => vi.fn());
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws" }));
vi.mock("../../platform", () => ({ openExternal }));

const chat = { channel_type: "feishu", chat_id: "oc_1", app_link: "https://applink.feishu.cn/client/chat/open?openChatId=oc_1", created_at: "" };

function setup(state: IssueChannelChat) {
  const getIssueChannelChat = vi.fn(async () => state);
  const createIssueChannelChat = vi.fn(async () => ({ available: true, can_create: false, chat }));
  setApiInstance({ getIssueChannelChat, createIssueChannelChat } as unknown as ApiClient);
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={qc}>
      <I18nProvider locale="en" resources={RESOURCES}>
        <IssueChannelChatButton issueId="issue-1" />
      </I18nProvider>
    </QueryClientProvider>,
  );
  return { getIssueChannelChat, createIssueChannelChat };
}

afterEach(() => {
  cleanup();
  openExternal.mockReset();
});

describe("IssueChannelChatButton", () => {
  it("stays hidden when no bot can host the issue's chat", async () => {
    const api = setup({ available: true, can_create: false, chat: null });
    await waitFor(() => expect(api.getIssueChannelChat).toHaveBeenCalledWith("issue-1"));
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("stays hidden when the deployment cannot manage chats", async () => {
    const api = setup({ available: false, can_create: false, chat: null });
    await waitFor(() => expect(api.getIssueChannelChat).toHaveBeenCalled());
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("opens an existing chat", async () => {
    setup({ available: true, can_create: false, chat });
    fireEvent.click(await screen.findByRole("button", { name: "Open Feishu group" }));
    expect(openExternal).toHaveBeenCalledWith(chat.app_link);
  });

  it("creates the chat and opens it", async () => {
    const api = setup({ available: true, can_create: true, chat: null });
    fireEvent.click(await screen.findByRole("button", { name: "Create Feishu group" }));
    await waitFor(() => expect(api.createIssueChannelChat).toHaveBeenCalledWith("issue-1"));
    await waitFor(() => expect(openExternal).toHaveBeenCalledWith(chat.app_link));
    expect(await screen.findByRole("button", { name: "Open Feishu group" })).toBeTruthy();
  });
});
