package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	"github.com/multica-ai/multica/server/internal/integrations/lark"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// RetryChannelMessageSync repairs the existing comment without re-running
// mention routing or emitting another comment:created event.
func (h *Handler) RetryChannelMessageSync(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	wsID := h.resolveWorkspaceID(r)
	member, ok := h.workspaceMember(w, r, wsID)
	if !ok {
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, wsID, "workspace id")
	if !ok {
		return
	}
	source, err := h.Queries.GetChannelIssueMessageByChannelID(r.Context(), chi.URLParam(r, "messageID"))
	if err != nil || source.WorkspaceID != wsUUID || source.Direction != "inbound" || !source.CommentID.Valid || len(source.SyncState) == 0 {
		writeError(w, http.StatusNotFound, "message sync not found")
		return
	}
	if _, ok := h.loadIssueForUser(w, r, uuidToString(source.IssueID)); !ok {
		return
	}
	existing, err := h.Queries.GetCommentInWorkspace(r.Context(), db.GetCommentInWorkspaceParams{ID: source.CommentID, WorkspaceID: wsUUID})
	if err != nil || existing.DeletedAt.Valid {
		writeError(w, http.StatusNotFound, "comment not found")
		return
	}
	if (existing.AuthorType != "member" || uuidToString(existing.AuthorID) != userID) && !roleAllowed(member.Role, "owner", "admin") {
		writeError(w, http.StatusForbidden, "only comment author or admin can retry sync")
		return
	}
	var state lark.ForwardedCommentState
	if json.Unmarshal(source.SyncState, &state) != nil {
		writeError(w, http.StatusInternalServerError, "invalid message sync state")
		return
	}
	if !state.Incomplete {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if state.Content != existing.Content {
		writeError(w, http.StatusConflict, "comment was edited; sync cannot replace it")
		return
	}
	if h.LarkIssueComments == nil {
		writeError(w, http.StatusServiceUnavailable, "Feishu sync is unavailable")
		return
	}
	store := lark.NewChannelStore(h.Queries)
	inst, err := store.GetLarkInstallationInWorkspace(r.Context(), lark.GetInstallationInWorkspaceParams{ID: source.InstallationID, WorkspaceID: wsUUID})
	if err != nil || lark.InstallationStatus(inst.Status) != lark.InstallationActive {
		writeError(w, http.StatusConflict, "Feishu connection is unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), engine.DefaultMediaTimeout+10*time.Second)
	defer cancel()
	result, err := h.LarkIssueComments.PrepareForwardedComment(ctx, engine.ResolvedInstallation{ID: inst.ID, WorkspaceID: wsUUID, Platform: inst}, state.Message, source.SyncState)
	if err != nil {
		writeError(w, http.StatusBadGateway, "could not retry message sync")
		return
	}
	tx, err := h.beginWakeupWrite(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not save message sync")
		return
	}
	defer tx.Rollback(ctx)
	q := h.Queries.WithTx(tx)
	updated, err := q.UpdateComment(ctx, db.UpdateCommentParams{
		ID: existing.ID, Content: result.Content, SourceTaskID: existing.SourceTaskID,
		ExpectedRevision:   pgtype.Int8{Int64: existing.Revision, Valid: true},
		SuppressedAgentIds: existing.SuppressedAgentIds,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusConflict, "comment changed during sync; retry")
		return
	}
	if err != nil {
		slog.Warn("channel sync comment update failed", "message_id", source.ChannelMessageID, "error", err)
		writeError(w, http.StatusInternalServerError, "could not save message sync")
		return
	}
	created := db.CreateCommentRow{ID: existing.ID, Revision: updated.Revision, IssueRevision: updated.IssueRevision}
	err = bindChannelIssueAttachments(ctx, q, lark.ChannelIssueCommentParams{
		WorkspaceID: wsUUID, IssueID: source.IssueID, AuthorUserID: existing.AuthorID, Attachments: result.Attachments,
	}, &created)
	if err == nil {
		err = q.SetChannelIssueMessageSyncState(ctx, db.SetChannelIssueMessageSyncStateParams{ChannelMessageID: source.ChannelMessageID, WorkspaceID: wsUUID, SyncState: result.SyncState})
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		slog.Warn("channel sync attachment commit failed", "message_id", source.ChannelMessageID, "error", err)
		writeError(w, http.StatusInternalServerError, "could not save message sync")
		return
	}
	comment, err := h.Queries.GetCommentInWorkspace(ctx, db.GetCommentInWorkspaceParams{ID: existing.ID, WorkspaceID: wsUUID})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load synced comment")
		return
	}
	attachments := h.groupAttachments(r, []pgtype.UUID{comment.ID})
	reactions := h.groupReactions(r, []pgtype.UUID{comment.ID})
	resp := commentToResponse(comment, reactions[uuidToString(comment.ID)], attachments[uuidToString(comment.ID)])
	resp.IssueRevision = created.IssueRevision
	h.publish(protocol.EventCommentUpdated, wsID, "member", userID, map[string]any{
		"comment": resp, "issue_revision": created.IssueRevision,
		lark.CommentChannelOriginKey: lark.CommentChannelOrigin{ChannelType: "feishu", InstallationID: uuidToString(source.InstallationID), MessageID: source.ChannelMessageID},
	})
	w.WriteHeader(http.StatusNoContent)
}
