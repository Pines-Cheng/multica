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

type fakeProjects struct{ projects []db.Project }

func (f fakeProjects) ListProjectsByTitle(context.Context, db.ListProjectsByTitleParams) ([]db.Project, error) {
	return f.projects, nil
}

func TestExtractProjectDirective(t *testing.T) {
	rest, name := extractProjectDirective("/project  Hello Demo \nadd an endpoint\nsee /project docs")
	if name != "Hello Demo" || rest != "add an endpoint\nsee /project docs" {
		t.Fatalf("rest=%q name=%q", rest, name)
	}
	if _, name := extractProjectDirective("mention /projects here\n/projectx y"); name != "" {
		t.Fatalf("non-directive lines matched %q", name)
	}
}

// A /project line files the issue under the one project of that title and
// never reaches the description; an unknown title files it without one.
func TestRouter_IssueProjectDirective(t *testing.T) {
	for _, tc := range []struct {
		name     string
		projects []db.Project
		want     bool
		miss     string
	}{
		{"one match", []db.Project{{ID: uid(95), Title: "Hello Demo"}}, true, ""},
		{"no match", nil, false, "not_found"},
		{"ambiguous", []db.Project{{ID: uid(96)}, {ID: uid(97)}}, false, "ambiguous"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.binder.parseIssue = true
			h.router = NewRouter(h.issues, h.tasks, h.reader, RouterConfig{Logger: discardLogger(), Lifecycle: h.lifecycle, Projects: fakeProjects{tc.projects}})
			set := ResolverSet{Installation: h.inst, Identity: h.ident, Dedup: h.dedup, Session: h.binder,
				Audit: h.audit, Replier: h.replier, Typing: h.typing, Media: h.media, OriginType: "lark_chat", IssueProjectDirective: true}
			h.router.Register(channel.TypeFeishu, set)
			msg := p2pMessage(t)
			msg.Text = "/issue Add hello\n/project hello demo\nadd an endpoint"
			msg.CommandText = msg.Text
			res, _, err := h.router.dispatch(context.Background(), set, msg, false, false)
			if err != nil {
				t.Fatalf("dispatch: %v", err)
			}
			if got := h.issues.params.ProjectID.Valid; got != tc.want {
				t.Fatalf("project set = %v, want %v", got, tc.want)
			}
			if h.issues.params.Description.String != "add an endpoint" {
				t.Fatalf("description = %q", h.issues.params.Description.String)
			}
			if res.IssueProjectMiss != tc.miss {
				t.Fatalf("miss = %q, want %q", res.IssueProjectMiss, tc.miss)
			}
		})
	}
}
