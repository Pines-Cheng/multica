package engine

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

type fakeIssueChat struct {
	mu        sync.Mutex
	route     IssueChatRoute
	bound     bool
	lookupErr error
	fromBot   bool
	recordErr error
	recorded  []ResolvedIdentity
}

func (f *fakeIssueChat) LookupIssueChat(context.Context, channel.InboundMessage) (IssueChatRoute, bool, error) {
	return f.route, f.bound, f.lookupErr
}

func (f *fakeIssueChat) SentByBot(channel.InboundMessage) bool { return f.fromBot }

func (f *fakeIssueChat) RecordIssueComment(_ context.Context, _ ResolvedInstallation, identity ResolvedIdentity, _ IssueChatRoute, _ channel.InboundMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.recordErr != nil {
		return f.recordErr
	}
	f.recorded = append(f.recorded, identity)
	return nil
}

func (f *fakeIssueChat) records() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.recorded)
}

// newIssueChatHarness is the standard harness with an issue-bound chat whose
// steward is the harness installation.
func newIssueChatHarness(t *testing.T) (*harness, *fakeIssueChat) {
	t.Helper()
	h := newHarness(t)
	ic := &fakeIssueChat{bound: true, route: IssueChatRoute{
		BindingID:             uid(201),
		WorkspaceID:           activeResolved(t).WorkspaceID,
		IssueID:               uid(202),
		StewardInstallationID: activeResolved(t).ID,
	}}
	h.router.Register(channel.TypeFeishu, ResolverSet{
		Installation: h.inst,
		Identity:     h.ident,
		Dedup:        h.dedup,
		Session:      h.binder,
		Audit:        h.audit,
		Replier:      h.replier,
		Typing:       h.typing,
		Media:        h.media,
		IssueChat:    ic,
		OriginType:   "lark_chat",
	})
	return h, ic
}

func groupMessage(t *testing.T, addressed bool) channel.InboundMessage {
	msg := p2pMessage(t)
	msg.Source.ChatType = channel.ChatTypeGroup
	msg.AddressedToBot = addressed
	return msg
}

// Every person's message in a bound chat is the issue's timeline, addressed
// or not, and it never opens a Chat session or a chat run.
func TestRouter_IssueChat_UnaddressedMessageBecomesComment(t *testing.T) {
	h, ic := newIssueChatHarness(t)
	if err := h.router.Handle(context.Background(), groupMessage(t, false)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if ic.records() != 1 {
		t.Fatalf("expected one recorded comment, got %d", ic.records())
	}
	if h.binder.ensureCalls != 0 || h.tasks.wasCalled() {
		t.Fatal("a bound chat must not reach the Chat pipeline")
	}
	if h.dedup.marks() != 1 {
		t.Fatalf("expected the dedup claim to be marked, got %d", h.dedup.marks())
	}
}

// Several bots sit in one bound chat and each receives the messages that
// @ it; only the steward ingests, so one message is one comment.
func TestRouter_IssueChat_NonStewardStandsDown(t *testing.T) {
	h, ic := newIssueChatHarness(t)
	ic.route.StewardInstallationID = uid(250)
	if err := h.router.Handle(context.Background(), groupMessage(t, true)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if ic.records() != 0 {
		t.Fatal("a non-steward installation must not record a comment")
	}
	if r, _ := h.audit.last(); r != DropReasonIssueChatNotSteward {
		t.Fatalf("expected %q, got %q", DropReasonIssueChatNotSteward, r)
	}
	if h.binder.ensureCalls != 0 {
		t.Fatal("a non-steward must not fall through to the Chat pipeline")
	}
}

// Bots mirror the issue timeline into the chat; ingesting their messages
// would loop comments back into the issue.
func TestRouter_IssueChat_BotMessageIsDropped(t *testing.T) {
	h, ic := newIssueChatHarness(t)
	ic.fromBot = true
	if err := h.router.Handle(context.Background(), groupMessage(t, true)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if ic.records() != 0 {
		t.Fatal("a bot message must not become a comment")
	}
	if r, _ := h.audit.last(); r != DropReasonIssueChatBotSender {
		t.Fatalf("expected %q, got %q", DropReasonIssueChatBotSender, r)
	}
}

// An unbound person chatting in the group is dropped quietly; only one who
// addresses the bot is offered the binding prompt.
func TestRouter_IssueChat_UnboundSender(t *testing.T) {
	for _, tc := range []struct {
		name      string
		addressed bool
	}{{"unaddressed", false}, {"addressed", true}} {
		t.Run(tc.name, func(t *testing.T) {
			h, ic := newIssueChatHarness(t)
			h.ident.err = ErrSenderUnbound
			res, _, err := h.router.dispatch(context.Background(), h.router.sets[channel.TypeFeishu], groupMessage(t, tc.addressed), false, false)
			if err != nil {
				t.Fatalf("dispatch: %v", err)
			}
			want := OutcomeDropped
			if tc.addressed {
				want = OutcomeNeedsBinding
			}
			if res.Outcome != want {
				t.Fatalf("outcome = %q, want %q", res.Outcome, want)
			}
			if ic.records() != 0 {
				t.Fatal("an unbound sender must not record a comment")
			}
		})
	}
}

func TestRouter_IssueChat_RecordFailureReleasesClaim(t *testing.T) {
	h, ic := newIssueChatHarness(t)
	ic.recordErr = errors.New("db down")
	if err := h.router.Handle(context.Background(), groupMessage(t, false)); err == nil {
		t.Fatal("expected an infrastructure error so the platform redelivers")
	}
	if h.dedup.releases() != 1 {
		t.Fatalf("expected the claim to be released for redelivery, got %d", h.dedup.releases())
	}
}

// An unbound chat keeps the ordinary group behavior.
func TestRouter_IssueChat_UnboundChatKeepsChatPipeline(t *testing.T) {
	h, ic := newIssueChatHarness(t)
	ic.bound = false
	ic.route = IssueChatRoute{}
	if err := h.router.Handle(context.Background(), groupMessage(t, false)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if r, _ := h.audit.last(); r != DropReasonNotAddressedInGroup {
		t.Fatalf("expected the ordinary group filter, got %q", r)
	}
	var zero pgtype.UUID
	if ic.route.IssueID != zero || ic.records() != 0 {
		t.Fatal("an unbound chat must not record a comment")
	}
}
