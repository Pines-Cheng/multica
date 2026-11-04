// @vitest-environment node
import { afterEach, expect, it, vi } from "vitest";
import { ApiClient } from "./client";

afterEach(() => vi.unstubAllGlobals());

it("retries channel synchronization using POST and accepts an empty 204", async () => {
  const fetch = vi.fn().mockResolvedValue(new Response(null, { status: 204 }));
  vi.stubGlobal("fetch", fetch);
  const client = new ApiClient("https://api.example.test");
  await expect(client.retryChannelMessageSync("om_forward")).resolves.toBeUndefined();
  expect(fetch).toHaveBeenCalledWith("https://api.example.test/api/channel-messages/om_forward/retry", expect.objectContaining({ method: "POST" }));
});

it("preserves an edit conflict as a failed sync", async () => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify({ error: "comment was edited" }), { status: 409 })));
  await expect(new ApiClient("https://api.example.test").retryChannelMessageSync("om_forward")).rejects.toMatchObject({ status: 409 });
});
