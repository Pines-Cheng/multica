import { fireEvent, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { api } from "@multica/core/api";
import type { Comment, TimelineEntry } from "@multica/core/types";
import { renderWithI18n } from "../../test/i18n";
import { CommentAssistance } from "./comment-assistance";

vi.mock("@multica/core/api", () => ({ api: { setCommentAssistance: vi.fn() } }));

beforeEach(() => vi.clearAllMocks());

const comment: Comment = { id: "request-1", issue_id: "issue-1", author_type: "agent", author_id: "agent-1", content: "Please authenticate", type: "comment", parent_id: null, reactions: [], attachments: [], created_at: "2026-11-03T10:00:00Z", updated_at: "2026-11-03T10:00:00Z", resolved_at: null, resolved_by_type: null, resolved_by_id: null };

const entry: TimelineEntry = { type: "comment", id: "request-1", actor_type: "agent", actor_id: "agent-1", created_at: "2026-11-03T10:00:00Z" };

function renderRequest(status: "none" | "open" | "closed", canAct = true) {
  return renderWithI18n(<QueryClientProvider client={new QueryClient({ defaultOptions: { mutations: { retry: false } } })}>
    <CommentAssistance issueId="issue-1" entry={{ ...entry, assistance_status: status }} canAct={canAct} />
  </QueryClientProvider>);
}

describe("CommentAssistance", () => {
  it("ordinary mentions have no assistance controls", () => {
    renderRequest("none");
    expect(screen.queryByRole("button")).toBeNull();
  });
  it("explicit requests close independently of discussion resolution", async () => {
    vi.mocked(api.setCommentAssistance).mockResolvedValueOnce(comment);
    renderRequest("open");
    fireEvent.click(screen.getByRole("button", { name: "Mark as handled" }));
    await waitFor(() => expect(api.setCommentAssistance).toHaveBeenCalledWith(entry.id, "closed"));
  });
  it("closed requests expose an explicit reopen action", async () => {
    vi.mocked(api.setCommentAssistance).mockResolvedValueOnce(comment);
    renderRequest("closed");
    fireEvent.click(screen.getByRole("button", { name: "Reopen request" }));
    await waitFor(() => expect(api.setCommentAssistance).toHaveBeenLastCalledWith(entry.id, "open"));
  });
  it("read-only viewers can see status but cannot change it", () => {
    renderRequest("open", false);
    expect(screen.getByText("Needs assistance")).toBeTruthy();
    expect(screen.queryByRole("button")).toBeNull();
  });
});
