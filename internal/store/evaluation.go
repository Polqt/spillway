package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

var evaluationRunIDPattern = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,128}$`)
var corpusDigestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// EvaluationRun contains only safe aggregate evaluation metadata.
type EvaluationRun struct {
	RunID        string          `json:"run_id"`
	CreatedAt    time.Time       `json:"created_at"`
	CorpusSHA256 string          `json:"corpus_sha256"`
	Summary      json.RawMessage `json:"summary"`
}

// RecordEvaluation stores a bounded, content-free aggregate report.
func (s *Store) RecordEvaluation(ctx context.Context, run EvaluationRun) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !evaluationRunIDPattern.MatchString(run.RunID) || !corpusDigestPattern.MatchString(run.CorpusSHA256) || len(run.Summary) == 0 || len(run.Summary) > 256<<10 || !json.Valid(run.Summary) {
		return errors.New("evaluation run metadata is invalid")
	}
	encoded := strings.ToLower(string(run.Summary))
	for _, forbidden := range []string{`"prompt":`, `"completion":`, `"messages":`, `"authorization":`, `"api_key":`, `"local_key":`} {
		if strings.Contains(encoded, forbidden) {
			return fmt.Errorf("evaluation summary contains forbidden field %s", strings.TrimSuffix(strings.Trim(forbidden, `"`), ":"))
		}
	}
	if run.CreatedAt.IsZero() {
		run.CreatedAt = s.now()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO evaluation_runs (run_id, created_ns, corpus_sha256, summary_json) VALUES (?, ?, ?, ?)`, run.RunID, run.CreatedAt.UTC().UnixNano(), run.CorpusSHA256, string(run.Summary))
	if err != nil {
		return fmt.Errorf("record evaluation run: %w", err)
	}
	return nil
}

// EvaluationRuns returns the newest bounded set of safe aggregate reports.
func (s *Store) EvaluationRuns(ctx context.Context, limit int) (runs []EvaluationRun, retErr error) {
	if limit < 1 || limit > 100 {
		return nil, errors.New("evaluation run limit must be from 1 to 100")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT run_id, created_ns, corpus_sha256, summary_json FROM evaluation_runs ORDER BY created_ns DESC, run_id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("query evaluation runs: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close evaluation runs: %w", err))
		}
	}()
	runs = make([]EvaluationRun, 0, limit)
	for rows.Next() {
		var run EvaluationRun
		var created int64
		var summary string
		if err := rows.Scan(&run.RunID, &created, &run.CorpusSHA256, &summary); err != nil {
			return nil, fmt.Errorf("read evaluation run: %w", err)
		}
		run.CreatedAt = time.Unix(0, created).UTC()
		run.Summary = json.RawMessage(summary)
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read evaluation runs: %w", err)
	}
	return runs, nil
}
