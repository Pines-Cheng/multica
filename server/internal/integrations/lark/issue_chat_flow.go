package lark

// The workflow a progress card draws: the stage names an agent keeps in the
// issue's "flow" metadata, with each sub-issue filed under the stage it
// belongs to, so the card reads as one progress line instead of a list that
// grows with every rework round.

import (
	"encoding/json"
	"strconv"
	"strings"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// flowMetadataKey is the issue metadata key holding the workflow's stages.
// Metadata keys are ASCII.
const flowMetadataKey = "flow"

// flowStages reads the stage names from issue metadata: a JSON array of
// names, or one string separated by "→", ">" or commas.
func flowStages(raw []byte) []string {
	var meta map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &meta) != nil {
		return nil
	}
	value := meta[flowMetadataKey]
	var names []string
	if json.Unmarshal(value, &names) != nil {
		var joined string
		if json.Unmarshal(value, &joined) != nil {
			return nil
		}
		// The CLI may store a JSON array as its text.
		if json.Unmarshal([]byte(joined), &names) != nil {
			names = strings.FieldsFunc(joined, func(r rune) bool {
				return r == '→' || r == '>' || r == ',' || r == '，'
			})
		}
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, n)
		}
	}
	return out
}

type flowStage struct {
	name string
	// parts are the steps a stage runs in parallel: "代码评审·部署 PPE".
	parts    []string
	children []db.Issue
	// rounds counts the rework rounds filed under the stage.
	rounds int
}

func childClosed(c db.Issue) bool { return c.Status == "done" || c.Status == "cancelled" }

// placeChildren files each sub-issue under the stage one of whose parts its
// label names. A sub-issue naming no stage (a fix, a re-review) is rework of
// the latest stage that already has a sub-issue.
func placeChildren(names []string, children []db.Issue) []flowStage {
	stages := make([]flowStage, len(names))
	for i, n := range names {
		stages[i] = flowStage{name: n, parts: strings.Split(n, "·")}
	}
	last := -1
	for _, c := range children {
		label := childLabel(c)
		at := -1
	match:
		for i := range stages {
			for _, p := range stages[i].parts {
				if p = strings.TrimSpace(p); p != "" && strings.Contains(label, p) {
					at = i
					break match
				}
			}
		}
		if at < 0 {
			if last < 0 {
				continue
			}
			at = last
		}
		stages[at].children = append(stages[at].children, c)
		if isRework(label) {
			stages[at].rounds = max(stages[at].rounds, reworkRound(label))
		}
		last = max(last, at)
	}
	return stages
}

// isRework reports whether a sub-issue redoes earlier work.
func isRework(label string) bool {
	return strings.HasPrefix(label, "修复") || strings.HasPrefix(label, "复审") || strings.HasPrefix(label, "重新")
}

// reworkRound reads N from "第 N 轮", or counts the sub-issue as round 1.
func reworkRound(label string) int {
	if i := strings.Index(label, "第"); i >= 0 {
		rest := strings.TrimSpace(label[i+len("第"):])
		if j := strings.Index(rest, "轮"); j > 0 {
			if n, err := strconv.Atoi(strings.TrimSpace(rest[:j])); err == nil && n > 0 {
				return n
			}
		}
	}
	return 1
}

// currentStage is the first stage not yet behind the work. A stage is behind
// once all its sub-issues closed, or once a later stage has sub-issues (a
// stage done by a person, such as acceptance, has none of its own). It
// returns len(stages) when every stage is behind.
func currentStage(stages []flowStage) int {
	lastWithChildren := -1
	for i, st := range stages {
		if len(st.children) > 0 {
			lastWithChildren = i
		}
	}
	for i, st := range stages {
		if i < lastWithChildren {
			continue
		}
		if len(st.children) == 0 {
			return i
		}
		for _, c := range st.children {
			if !childClosed(c) {
				return i
			}
		}
	}
	return len(stages)
}

// flowLine renders the stages as one progress line: stages behind the work
// in grey, the current one in bold.
func flowLine(stages []flowStage, current int) string {
	parts := make([]string, len(stages))
	for i, st := range stages {
		name := st.name
		if st.rounds > 0 {
			name += "（返工 " + strconv.Itoa(st.rounds) + " 轮）"
		}
		switch {
		case i < current:
			parts[i] = "<font color='grey'>" + name + "</font>"
		case i == current:
			parts[i] = "**" + name + "**"
		default:
			parts[i] = name
		}
	}
	return strings.Join(parts, " → ")
}

// stageOf names the stage a sub-issue was filed under.
func stageOf(stages []flowStage, child db.Issue) string {
	for _, st := range stages {
		for _, c := range st.children {
			if c.ID == child.ID {
				return st.name
			}
		}
	}
	return ""
}
