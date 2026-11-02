package lark

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func flowChild(title, status string) db.Issue {
	return db.Issue{ID: pgtype.UUID{Bytes: uuid.New(), Valid: true}, Title: title + "：项目空间添加搜索功能", Status: status}
}

func TestFlowStagesReadsArrayOrSeparatedString(t *testing.T) {
	want := []string{"需求澄清", "开发", "代码评审·部署 PPE", "验收", "合入"}
	for _, raw := range []string{
		`{"flow":["需求澄清","开发","代码评审·部署 PPE","验收","合入"]}`,
		`{"flow":"需求澄清 → 开发 → 代码评审·部署 PPE → 验收 → 合入"}`,
		`{"flow":"[\"需求澄清\",\"开发\",\"代码评审·部署 PPE\",\"验收\",\"合入\"]"}`,
	} {
		if got := flowStages([]byte(raw)); strings.Join(got, "|") != strings.Join(want, "|") {
			t.Fatalf("flowStages(%s) = %q", raw, got)
		}
	}
	if got := flowStages([]byte(`{"MR":"https://x"}`)); got != nil {
		t.Fatalf("no flow key must give no stages, got %q", got)
	}
}

// The DEV-14 run: a review was sent back once, fixed and re-reviewed, and the
// work now waits on acceptance.
func TestFlowPlacesReworkUnderItsStage(t *testing.T) {
	names := []string{"需求澄清", "开发", "代码评审·部署 PPE", "验收", "合入"}
	children := []db.Issue{
		flowChild("需求澄清", "done"),
		flowChild("开发", "done"),
		flowChild("代码评审", "done"),
		flowChild("部署 PPE", "done"),
		flowChild("修复评审意见 第 1 轮", "done"),
		flowChild("复审 第 1 轮", "done"),
	}
	stages := placeChildren(names, children)
	if n := len(stages[2].children); n != 4 || stages[2].rounds != 1 {
		t.Fatalf("review stage holds %d sub-issues over %d rounds, want 4 over 1", n, stages[2].rounds)
	}
	current := currentStage(stages)
	if names[current] != "验收" {
		t.Fatalf("current stage = %q, want 验收", names[current])
	}
	line := flowLine(stages, current)
	for _, want := range []string{"<font color='grey'>代码评审·部署 PPE（返工 1 轮）</font>", "**验收**", "→ 合入"} {
		if !strings.Contains(line, want) {
			t.Fatalf("progress line %q lacks %q", line, want)
		}
	}

	// A running re-review keeps the review stage current.
	children[5].Status = "in_progress"
	stages = placeChildren(names, children)
	if current := currentStage(stages); names[current] != "代码评审·部署 PPE" {
		t.Fatalf("current stage = %q while the re-review runs", names[current])
	}
	if got := stageOf(stages, children[5]); got != "代码评审·部署 PPE" {
		t.Fatalf("re-review filed under %q", got)
	}
}
