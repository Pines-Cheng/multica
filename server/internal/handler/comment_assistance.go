package handler

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func (h *Handler) validateAssistanceRecipients(w http.ResponseWriter, r *http.Request, workspaceID pgtype.UUID, content string) bool {
	found := false
	for _, mention := range util.ParseMentions(content) {
		if mention.Type != "member" {
			continue
		}
		id, ok := parseUUIDOrBadRequest(w, mention.ID, "assistance recipient")
		if !ok {
			return false
		}
		if _, err := h.Queries.GetMemberByUserAndWorkspace(r.Context(), db.GetMemberByUserAndWorkspaceParams{UserID: id, WorkspaceID: workspaceID}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusBadRequest, "assistance recipient must be a workspace member")
			} else {
				writeError(w, http.StatusInternalServerError, "failed to validate assistance recipient")
			}
			return false
		}
		found = true
	}
	if !found {
		writeError(w, http.StatusBadRequest, "an assistance request must mention at least one workspace member")
	}
	return found
}

func (h *Handler) CloseCommentAssistance(w http.ResponseWriter, r *http.Request) {
	h.setCommentAssistance(w, r, "closed")
}

func (h *Handler) ReopenCommentAssistance(w http.ResponseWriter, r *http.Request) {
	h.setCommentAssistance(w, r, "open")
}

func (h *Handler) setCommentAssistance(w http.ResponseWriter, r *http.Request, status string) {
	r = h.withWakeupActor(r)
	comment, workspaceID, actorType, actorID, ok := h.loadCommentForActor(w, r)
	if !ok {
		return
	}
	if comment.AssistanceStatus == "none" {
		writeError(w, http.StatusConflict, "comment is not an assistance request")
		return
	}
	if status == "open" && !h.validateAssistanceRecipients(w, r, comment.WorkspaceID, comment.Content) {
		return
	}
	updated, err := wakeupWrite(h, r, func(q *db.Queries) (db.Comment, error) {
		return q.SetCommentAssistanceStatus(r.Context(), db.SetCommentAssistanceStatusParams{ID: comment.ID, WorkspaceID: comment.WorkspaceID, Status: status})
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "comment not found")
		} else {
			writeError(w, http.StatusInternalServerError, "failed to update assistance request")
		}
		return
	}
	cid := uuidToString(updated.ID)
	resp := commentToResponse(updated, h.groupReactions(r, []pgtype.UUID{updated.ID})[cid], h.groupAttachments(r, []pgtype.UUID{updated.ID})[cid])
	if updated.Revision != comment.Revision {
		h.publish(protocol.EventCommentUpdated, workspaceID, actorType, actorID, map[string]any{"comment": resp})
	}
	writeJSON(w, http.StatusOK, resp)
}
