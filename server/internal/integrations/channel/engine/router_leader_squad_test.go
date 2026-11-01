package engine

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type fakeSquads struct{ squads []db.Squad }

func (f fakeSquads) ListActiveSquadsLedByAgent(context.Context, db.ListActiveSquadsLedByAgentParams) ([]db.Squad, error) {
	return f.squads, nil
}

func leaderSquadHarness(t *testing.T, squads []db.Squad) *harness {
	t.Helper()
	h := newHarness(t)
	h.binder.parseIssue = true
	h.router = NewRouter(h.issues, h.tasks, h.reader, RouterConfig{Logger: discardLogger(), Lifecycle: h.lifecycle, Squads: fakeSquads{squads}})
	h.router.Register(channel.TypeFeishu, ResolverSet{
		Installation: h.inst, Identity: h.ident, Dedup: h.dedup, Session: h.binder,
		Audit: h.audit, Replier: h.replier, Typing: h.typing, Media: h.media,
		OriginType: "lark_chat", IssueToLeaderSquad: true,
	})
	return h
}

func issueCommandMessage(t *testing.T) channel.InboundMessage {
	msg := p2pMessage(t)
	msg.Text = "/issue Research competitors"
	msg.CommandText = msg.Text
	return msg
}

// D3: an /issue sent to the bot of an agent leading exactly one squad goes to
// that squad, so its leader coordinates instead of doing the work itself.
func TestRouter_IssueToTheOneSquadTheAgentLeads(t *testing.T) {
	h := leaderSquadHarness(t, []db.Squad{{ID: uid(90), Name: "Builders"}})
	if err := h.router.Handle(context.Background(), issueCommandMessage(t)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if h.issues.params.AssigneeType.String != "squad" || h.issues.params.AssigneeID != uid(90) {
		t.Fatalf("assignee = %s/%v, want squad", h.issues.params.AssigneeType.String, h.issues.params.AssigneeID)
	}
}

func TestRouter_IssueStaysWithTheAgentLeadingNoneOrSeveralSquads(t *testing.T) {
	for _, squads := range [][]db.Squad{nil, {{ID: uid(91)}, {ID: uid(92)}}} {
		h := leaderSquadHarness(t, squads)
		if err := h.router.Handle(context.Background(), issueCommandMessage(t)); err != nil {
			t.Fatalf("Handle: %v", err)
		}
		if h.issues.params.AssigneeType.String != "agent" || h.issues.params.AssigneeID != activeResolved(t).AgentID {
			t.Fatalf("squads=%d: assignee = %s, want the bot's agent", len(squads), h.issues.params.AssigneeType.String)
		}
	}
}
