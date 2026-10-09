package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cameronpyne-smith/reviewdo/internal/ollama"
	"github.com/cameronpyne-smith/reviewdo/internal/review"
)

const judgePrompt = `You compare two sets of code review findings on the same pull request: the reference set and the candidate set. Two findings match when they point at the same underlying problem, even if worded differently, anchored a few lines apart, or anchored on a different file that shows the same problem (for example a broken link reported at the link versus at the missing target). Findings that merely touch the same lines but describe different problems do not match. One finding matches at most one finding in the other set.

Reply with JSON only: {"pairs": [{"reference": "<id>", "candidate": "<id>"}]}. Leave pairs empty when nothing matches.`

var judgeSchema = json.RawMessage(`{"type":"object","properties":{"pairs":{"type":"array","items":{"type":"object","properties":{"reference":{"type":"string"},"candidate":{"type":"string"}},"required":["reference","candidate"]}}},"required":["pairs"]}`)

type pair struct {
	Reference string `json:"reference"`
	Candidate string `json:"candidate"`
}

func judge(ctx context.Context, llm *ollama.Client, golden []Finding, found []RunFinding) ([]pair, error) {
	if len(golden) == 0 || len(found) == 0 {
		return nil, nil
	}
	var b strings.Builder
	b.WriteString("Reference findings:\n")
	for i, f := range golden {
		fmt.Fprintf(&b, "\n[R%d] %s line %d\n%s\n", i+1, f.Path, f.Line, truncate(f.Body, 1500))
	}
	b.WriteString("\nCandidate findings:\n")
	for i, f := range found {
		fmt.Fprintf(&b, "\n[C%d] %s line %d (%s)\n%s\n", i+1, f.Path, f.Line, f.Severity, truncate(f.Body, 1500))
	}
	raw, _, err := review.RunSingle(ctx, llm, judgePrompt, b.String(), judgeSchema)
	if err != nil {
		return nil, err
	}
	var out struct {
		Pairs []pair `json:"pairs"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("bad judge output %q: %w", truncate(string(raw), 200), err)
	}
	var pairs []pair
	for _, p := range out.Pairs {
		ri, ci := index(p.Reference, "R"), index(p.Candidate, "C")
		if ri < 0 || ri >= len(golden) || ci < 0 || ci >= len(found) {
			continue
		}
		pairs = append(pairs, pair{Reference: golden[ri].ID, Candidate: found[ci].ID})
	}
	return pairs, nil
}

func index(s, prefix string) int {
	s = strings.TrimSpace(strings.Trim(s, "[]"))
	var n int
	if _, err := fmt.Sscanf(s, prefix+"%d", &n); err != nil {
		return -1
	}
	return n - 1
}
