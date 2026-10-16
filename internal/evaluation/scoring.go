package evaluation

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
)

// Score is one blinded human rating of a completion.
type Score struct {
	RaterID                string
	AnswerID               string
	Correctness            int
	Completeness           int
	InstructionFollowing   int
	SevereCorrectnessFault bool
}

// QualityMetrics summarizes blinded scores by arm and prompt difficulty.
type QualityMetrics struct {
	Arm                      string  `json:"arm"`
	Difficulty               string  `json:"difficulty"`
	Category                 string  `json:"category"`
	Responses                int     `json:"responses"`
	MeanScore                float64 `json:"mean_score"`
	MeanCorrectness          float64 `json:"mean_correctness"`
	MeanCompleteness         float64 `json:"mean_completeness"`
	MeanInstructionFollowing float64 `json:"mean_instruction_following"`
	SevereFailures           int     `json:"severe_failures"`
	SevereFailureRate        float64 `json:"severe_failure_rate"`
}

// LoadScores reads a completed score sheet and validates its rubric values.
func LoadScores(r io.Reader) ([]Score, error) {
	reader := csv.NewReader(r)
	reader.FieldsPerRecord = -1
	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("read score sheet header: %w", err)
	}
	want := []string{"answer_id", "correctness_1_to_5", "completeness_1_to_5", "instruction_following_1_to_5", "severe_correctness_failure", "adjudication_note"}
	raterIndex := -1
	if len(header) == len(want)+1 {
		for index, field := range header {
			if field == "rater_id" {
				if raterIndex >= 0 {
					return nil, errors.New("score sheet has an invalid header")
				}
				raterIndex = index
			}
		}
		if raterIndex != 0 && raterIndex != len(header)-1 {
			return nil, errors.New("score sheet has an invalid header")
		}
		withoutRater := append([]string(nil), header[:raterIndex]...)
		header = append(withoutRater, header[raterIndex+1:]...)
	}
	if len(header) != len(want) {
		return nil, errors.New("score sheet has an invalid header")
	}
	for i := range want {
		if header[i] != want[i] {
			return nil, errors.New("score sheet has an invalid header")
		}
	}
	var scores []Score
	seen := make(map[string]bool)
	for row := 2; ; row++ {
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read score row %d: %w", row, err)
		}
		raterID := ""
		if len(record) != len(want)+boolInt(raterIndex >= 0) {
			return nil, fmt.Errorf("score row %d has an invalid number of fields", row)
		}
		if raterIndex >= 0 {
			raterID = record[raterIndex]
			record = append(append([]string(nil), record[:raterIndex]...), record[raterIndex+1:]...)
			if !label(raterID) {
				return nil, fmt.Errorf("score row %d has an invalid rater id", row)
			}
		}
		seenKey := raterID + "\x00" + record[0]
		if record[0] == "" || seen[seenKey] {
			return nil, fmt.Errorf("score row %d has an empty or duplicate answer id", row)
		}
		seen[seenKey] = true
		values := [3]int{}
		for i, field := range record[1:4] {
			value, err := strconv.Atoi(field)
			if err != nil || value < 1 || value > 5 {
				return nil, fmt.Errorf("score row %d rubric values must be integers from 1 to 5", row)
			}
			values[i] = value
		}
		var severe bool
		switch record[4] {
		case "true":
			severe = true
		case "false":
		default:
			return nil, fmt.Errorf("score row %d severe correctness failure must be true or false", row)
		}
		scores = append(scores, Score{RaterID: raterID, AnswerID: record[0], Correctness: values[0], Completeness: values[1], InstructionFollowing: values[2], SevereCorrectnessFault: severe})
	}
	if len(scores) == 0 {
		return nil, errors.New("score sheet has no scored answers")
	}
	return scores, nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// SummarizeScores combines blinded scores with the separate assignment key.
func SummarizeScores(answers []Answer, assignments []Assignment, scores []Score) ([]QualityMetrics, error) {
	type groupKey struct {
		arm, difficulty, category string
	}
	answerByID := make(map[string]Answer, len(answers))
	for _, answer := range answers {
		answerByID[answer.AnswerID] = answer
	}
	armByID := make(map[string]string, len(assignments))
	for _, assignment := range assignments {
		armByID[assignment.AnswerID] = assignment.Arm
	}
	type group struct {
		arm, difficulty, category            string
		count, severe                        int
		correctness, completeness, following int
	}
	groups := make(map[groupKey]*group)
	add := func(arm, difficulty, category string, score Score) {
		key := groupKey{arm, difficulty, category}
		g := groups[key]
		if g == nil {
			g = &group{arm: arm, difficulty: difficulty, category: category}
			groups[key] = g
		}
		g.count++
		g.correctness += score.Correctness
		g.completeness += score.Completeness
		g.following += score.InstructionFollowing
		if score.SevereCorrectnessFault {
			g.severe++
		}
	}
	seen := make(map[string]bool)
	for _, score := range scores {
		answer, ok := answerByID[score.AnswerID]
		arm, hasArm := armByID[score.AnswerID]
		if !ok || !hasArm || seen[score.AnswerID] {
			return nil, fmt.Errorf("score for unknown or repeated answer %q", score.AnswerID)
		}
		seen[score.AnswerID] = true
		add(arm, answer.Difficulty, answer.Category, score)
		add(arm, answer.Difficulty, "all", score)
		add(arm, "all", answer.Category, score)
		add(arm, "all", "all", score)
	}
	if len(seen) != len(answers) {
		return nil, fmt.Errorf("score sheet contains %d of %d answers", len(seen), len(answers))
	}
	result := make([]QualityMetrics, 0, len(groups))
	for _, g := range groups {
		responses := float64(g.count)
		correctness := float64(g.correctness) / responses
		completeness := float64(g.completeness) / responses
		following := float64(g.following) / responses
		result = append(result, QualityMetrics{
			Arm: g.arm, Difficulty: g.difficulty, Category: g.category, Responses: g.count,
			MeanScore:       (correctness + completeness + following) / 3,
			MeanCorrectness: correctness, MeanCompleteness: completeness, MeanInstructionFollowing: following,
			SevereFailures: g.severe, SevereFailureRate: float64(g.severe) / responses,
		})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Arm != result[j].Arm {
			return result[i].Arm < result[j].Arm
		}
		if result[i].Difficulty != result[j].Difficulty {
			return result[i].Difficulty < result[j].Difficulty
		}
		return result[i].Category < result[j].Category
	})
	return result, nil
}
