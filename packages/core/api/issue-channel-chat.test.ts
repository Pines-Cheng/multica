// @vitest-environment node
import { afterEach, expect, it, vi } from "vitest";
import { ApiClient } from "./client";

afterEach(() => vi.unstubAllGlobals());
const client = new ApiClient("https://api.example.test");

function respond(body: unknown) {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify(body))));
}

it("reads a malformed chat as no chat", async () => {
  respond({ available: true, can_create: true, chat: { chat_id: 42 } });
  await expect(client.getIssueChannelChat("issue")).resolves.toEqual({ available: true, can_create: true, chat: null });
});

it("reads an older server's missing payload as unavailable", async () => {
  respond({});
  await expect(client.getIssueChannelChat("issue")).resolves.toEqual({ available: false, can_create: false, chat: null });
});

it("defaults the channel type of a bound chat", async () => {
  respond({ available: true, can_create: false, chat: { chat_id: "oc_1", app_link: "https://applink.feishu.cn/x" } });
  const res = await client.getIssueChannelChat("issue");
  expect(res.chat).toEqual({ channel_type: "feishu", chat_id: "oc_1", app_link: "https://applink.feishu.cn/x", created_at: "" });
});

it("does not report a create without a chat as success", async () => {
  respond({ available: true, can_create: false, chat: null });
  await expect(client.createIssueChannelChat("issue")).rejects.toThrow("Could not create the issue chat");
});
