import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ChannelSyncRetry, channelSyncMessageId } from "./channel-sync-retry";
import { api } from "@multica/core/api";

vi.mock("@multica/core/api", () => ({ api: { retryChannelMessageSync: vi.fn() } }));
vi.mock("../i18n", async () => {
  const editor = (await import("../locales/en/editor.json")).default;
  return { useT: () => ({ t: (select: (bundle: typeof editor) => string) => select(editor) }) };
});

describe("forwarded message sync retry", () => {
  it("accepts only a local, exact sync endpoint", () => {
    expect(channelSyncMessageId("/api/channel-messages/om_forward/retry")).toBe("om_forward");
    for (const href of [undefined, "https://other.test/api/channel-messages/om_forward/retry", "//other.test/api/channel-messages/om_forward/retry", "/api/channel-messages/../../retry", "/api/channel-messages/om_forward/retry?x=1"]) {
      expect(channelSyncMessageId(href)).toBeUndefined();
    }
  });

  it("waits for retry and refreshes the timeline; failures remain actionable", async () => {
    const client = new QueryClient();
    const invalidate = vi.spyOn(client, "invalidateQueries");
    let finish!: () => void;
    vi.mocked(api.retryChannelMessageSync).mockImplementationOnce(() => new Promise<void>((resolve) => { finish = resolve; }));
    render(<QueryClientProvider client={client}><ChannelSyncRetry messageId="om_forward" /></QueryClientProvider>);
    fireEvent.click(screen.getByRole("button", { name: "Retry sync" }));
    expect(screen.getByRole("button", { name: "Syncing..." })).toBeDisabled();
    expect(invalidate).not.toHaveBeenCalled();
    finish();
    await waitFor(() => expect(invalidate).toHaveBeenCalledWith({ queryKey: ["issues", "timeline"] }));
    await waitFor(() => expect(screen.getByRole("button", { name: "Retry sync" })).toBeEnabled());
    vi.mocked(api.retryChannelMessageSync).mockRejectedValueOnce(new Error("permission denied"));
    fireEvent.click(screen.getByRole("button", { name: "Retry sync" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("Could not sync");
    expect(screen.getByRole("button", { name: "Retry sync" })).toBeEnabled();
  });
});
