package execenv

import (
	"strings"
	"testing"
)

func TestGeneratedDocumentDeliveryRuleInIssueAndCommentBriefs(t *testing.T) {
	for _, ctx := range []TaskContextForEnv{{IssueID: "issue"}, {IssueID: "issue", TriggerCommentID: "comment"}, {IssueID: "issue", IsSquadLeader: true}} {
		brief := buildMetaSkillContent("claude", ctx)
		for _, want := range []string{"artifact set --generated", "before any delivery link", "--reference", "On failure, report access as not ready"} {
			if !strings.Contains(brief, want) {
				t.Errorf("brief is missing %q", want)
			}
		}
	}
}
