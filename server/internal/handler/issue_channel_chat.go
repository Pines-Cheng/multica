package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/lark"
	"github.com/multica-ai/multica/server/internal/logger"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type commentChannelOriginKey struct{}
type channelIssueCommentKey struct{}

func withCommentChannelOrigin(ctx context.Context, origin lark.CommentChannelOrigin) context.Context {
	return context.WithValue(ctx, commentChannelOriginKey{}, origin)
}

func commentChannelOrigin(ctx context.Context) (lark.CommentChannelOrigin, bool) {
	origin, ok := ctx.Value(commentChannelOriginKey{}).(lark.CommentChannelOrigin)
	return origin, ok
}

var _ lark.IssueCommentWriter = (*Handler)(nil)

// CreateChannelIssueComment writes a comment a member posted in an
// issue-bound chat. It runs the comment endpoint in-process with the member's
// identity, so access checks, mention triggers and events are exactly those
// of a comment written on the web; the channel origin rides on the context
// into the comment:created payload.
func (h *Handler) CreateChannelIssueComment(ctx context.Context, p lark.ChannelIssueCommentParams) (pgtype.UUID, error) {
	issueID := uuidToString(p.IssueID)
	body := map[string]any{"content": p.Content}
	if p.ParentCommentID.Valid {
		body["parent_id"] = uuidToString(p.ParentCommentID)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return pgtype.UUID{}, err
	}
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", issueID)
	reqCtx := context.WithValue(withCommentChannelOrigin(ctx, p.Origin), chi.RouteCtxKey, rctx)
	reqCtx = context.WithValue(reqCtx, channelIssueCommentKey{}, p)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, "/api/issues/"+issueID+"/comments", bytes.NewReader(raw))
	if err != nil {
		return pgtype.UUID{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-ID", uuidToString(p.AuthorUserID))
	req.Header.Set("X-Workspace-ID", uuidToString(p.WorkspaceID))

	rec := httptest.NewRecorder()
	h.CreateComment(rec, req)
	switch {
	case rec.Code == http.StatusCreated:
		var resp CommentResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			return pgtype.UUID{}, fmt.Errorf("decode channel comment response: %w", err)
		}
		return parseUUID(resp.ID), nil
	case rec.Code >= 400 && rec.Code < 500:
		return pgtype.UUID{}, fmt.Errorf("%w: status %d: %s", lark.ErrIssueCommentRejected, rec.Code, rec.Body.String())
	default:
		return pgtype.UUID{}, fmt.Errorf("create channel comment: status %d: %s", rec.Code, rec.Body.String())
	}
}

// bindChannelIssueComment commits attachments, the source-message mapping and
// queue acknowledgement with the comment, before events or agent triggers run.
func bindChannelIssueComment(ctx context.Context, q *db.Queries, p lark.ChannelIssueCommentParams, created *db.CreateCommentRow) error {
	if err := bindChannelIssueAttachments(ctx, q, p, created); err != nil {
		return err
	}
	if err := q.RecordChannelIssueMessage(ctx, db.RecordChannelIssueMessageParams{
		WorkspaceID: p.WorkspaceID, IssueID: p.IssueID, BindingID: p.BindingID,
		InstallationID: parseUUID(p.Origin.InstallationID), ChannelMessageID: p.Origin.MessageID,
		Direction: "inbound", Kind: "comment", CommentID: created.ID,
	}); err != nil {
		return err
	}
	if len(p.SyncState) > 0 {
		if err := q.SetChannelIssueMessageSyncState(ctx, db.SetChannelIssueMessageSyncStateParams{
			ChannelMessageID: p.Origin.MessageID, WorkspaceID: p.WorkspaceID, SyncState: p.SyncState,
		}); err != nil {
			return err
		}
	}
	count, err := q.FinishChannelIssueComment(ctx, db.FinishChannelIssueCommentParams{
		ChannelMessageID: p.Origin.MessageID, WorkspaceID: p.WorkspaceID, LeaseToken: p.LeaseToken,
	})
	if err != nil {
		return err
	}
	if count != 1 {
		return errors.New("channel comment queue claim lost")
	}
	return nil
}

// IssueChannelChatResponse describes an issue's IM chat.
type IssueChannelChatResponse struct {
	// Available is false when the deployment cannot manage Feishu chats.
	Available bool `json:"available"`
	// CanCreate reports whether opening a chat would find a host bot: the
	// squad leader's or assignee agent's connected Feishu bot.
	CanCreate bool                   `json:"can_create"`
	Chat      *IssueChannelChatEntry `json:"chat"`
}

type IssueChannelChatEntry struct {
	ChannelType string `json:"channel_type"`
	ChatID      string `json:"chat_id"`
	AppLink     string `json:"app_link"`
	CreatedAt   string `json:"created_at"`
}

func issueChannelChatEntry(chat lark.IssueChat) *IssueChannelChatEntry {
	return &IssueChannelChatEntry{
		ChannelType: "feishu",
		ChatID:      chat.ChatID,
		AppLink:     chat.AppLink,
		CreatedAt:   chat.CreatedAt.UTC().Format(time.RFC3339),
	}
}

// GetIssueChannelChat — GET /api/issues/{id}/channel-chat
func (h *Handler) GetIssueChannelChat(w http.ResponseWriter, r *http.Request) {
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	resp := IssueChannelChatResponse{}
	if h.LarkIssueChat == nil {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp.Available = true
	chat, bound, err := h.LarkIssueChat.GetChat(r.Context(), issue.ID)
	if err != nil {
		slog.Warn("get issue channel chat failed", append(logger.RequestAttrs(r), "error", err, "issue_id", uuidToString(issue.ID))...)
		writeError(w, http.StatusInternalServerError, "failed to load issue chat")
		return
	}
	if bound {
		resp.Chat = issueChannelChatEntry(chat)
	} else if resp.CanCreate, err = h.LarkIssueChat.CanHost(r.Context(), issue); err != nil {
		slog.Warn("check issue chat host failed", append(logger.RequestAttrs(r), "error", err, "issue_id", uuidToString(issue.ID))...)
	}
	writeJSON(w, http.StatusOK, resp)
}

// CreateIssueChannelChatRequest optionally names an existing chat to bind
// instead of creating one.
type CreateIssueChannelChatRequest struct {
	ChatID string `json:"chat_id"`
}

// CreateIssueChannelChat — POST /api/issues/{id}/channel-chat. Without a
// body it creates the issue's chat; with {"chat_id": "oc_..."} it binds that
// existing chat. Idempotent: an issue that already has the chat returns it.
func (h *Handler) CreateIssueChannelChat(w http.ResponseWriter, r *http.Request) {
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	var req CreateIssueChannelChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ChatID != "" && !validFeishuChatID(req.ChatID) {
		writeErrorCode(w, http.StatusBadRequest, "invalid_chat_id", "chat_id must be a Feishu chat id (oc_...)")
		return
	}
	if h.LarkIssueChat == nil {
		writeFeatureDisabled(w, "lark_not_configured", "feishu integration is not configured")
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	chat, err := h.openIssueChannelChat(r.Context(), issue, parseUUID(userID), req.ChatID)
	if err != nil {
		status, code, msg := issueChannelChatError(err)
		if code == "" {
			slog.Warn("create issue channel chat failed", append(logger.RequestAttrs(r), "error", err, "issue_id", uuidToString(issue.ID))...)
			writeError(w, status, msg)
			return
		}
		writeErrorCode(w, status, code, msg)
		return
	}
	writeJSON(w, http.StatusOK, IssueChannelChatResponse{Available: true, Chat: issueChannelChatEntry(chat)})
}

// openIssueChannelChat binds chatID when given, else creates the issue's chat.
func (h *Handler) openIssueChannelChat(ctx context.Context, issue db.Issue, actor pgtype.UUID, chatID string) (lark.IssueChat, error) {
	if chatID != "" {
		return h.LarkIssueChat.BindChat(ctx, lark.BindIssueChatParams{
			WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, ActorUserID: actor, ChatID: chatID,
		})
	}
	return h.LarkIssueChat.EnsureChat(ctx, lark.EnsureIssueChatParams{
		WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, ActorUserID: actor,
	})
}

// validFeishuChatID accepts Lark group chat ids.
func validFeishuChatID(chatID string) bool {
	return strings.HasPrefix(chatID, "oc_") && len(chatID) > len("oc_") && len(chatID) <= 128
}

// issueChannelChatError maps a chat open/bind failure to an HTTP status and
// a stable code. An empty code is an unexpected failure.
func issueChannelChatError(err error) (int, string, string) {
	var apiErr *lark.APIError
	switch {
	case errors.Is(err, lark.ErrIssueChatUnavailable):
		return http.StatusForbidden, "lark_chat_unavailable", "feishu chat management is not available"
	case errors.Is(err, lark.ErrIssueChatNoSteward):
		return http.StatusConflict, "no_steward_bot", "the issue's squad leader or assignee agent has no connected Feishu bot"
	case errors.Is(err, lark.ErrIssueChatIssueBound):
		return http.StatusConflict, "issue_chat_already_bound", "the issue is already bound to another chat"
	case errors.Is(err, lark.ErrIssueChatTaken):
		return http.StatusConflict, "chat_already_bound", "the chat is already bound to another issue"
	case errors.Is(err, lark.ErrIssueChatBotNotInChat):
		return http.StatusConflict, "steward_bot_not_in_chat", "add the issue's steward bot (squad leader or assignee agent) to the chat first"
	case errors.Is(err, lark.ErrIssueChatExternal):
		return http.StatusUnprocessableEntity, "external_chat_unsupported", "external chats cannot be bound to an issue"
	case errors.Is(err, lark.ErrChatNotFound):
		return http.StatusNotFound, "chat_not_found", "the Feishu chat does not exist"
	case errors.As(err, &apiErr):
		return http.StatusBadGateway, "lark_api_error", apiErr.Error()
	}
	return http.StatusInternalServerError, "", "failed to create issue chat"
}

func bindChannelIssueAttachments(ctx context.Context, q *db.Queries, p lark.ChannelIssueCommentParams, created *db.CreateCommentRow) error {
	keys := make([]string, 0, len(p.Attachments))
	for _, attachment := range p.Attachments {
		keys = append(keys, attachment.StorageKey)
	}
	if len(keys) > 0 {
		claimed, err := q.ClaimChannelMediaPendingObjectsForBind(ctx, db.ClaimChannelMediaPendingObjectsForBindParams{
			StorageKeys: keys, WorkspaceID: p.WorkspaceID,
		})
		if err != nil {
			return err
		}
		if len(claimed) != len(keys) {
			return errors.New("channel comment media is no longer available")
		}
	}
	for _, attachment := range p.Attachments {
		contentType := attachment.MimeType
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		row, err := q.CreateAttachment(ctx, db.CreateAttachmentParams{
			ID: attachment.ID, WorkspaceID: p.WorkspaceID, IssueID: p.IssueID, CommentID: created.ID,
			UploaderType: "member", UploaderID: p.AuthorUserID, Filename: attachment.Filename,
			Url: attachment.StorageURL, ContentType: contentType, SizeBytes: attachment.SizeBytes,
		})
		if err != nil {
			return err
		}
		created.Revision = row.CommentRevision
		created.IssueRevision = row.IssueRevision
	}
	return nil
}
