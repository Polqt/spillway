package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrInvalidCursor reports a malformed opaque recent-request cursor.
var ErrInvalidCursor = errors.New("invalid recent cursor")

// UsageQuery selects a bounded date range and one supported grouping.
type UsageQuery struct {
	Since time.Time
	Until time.Time
	Group string
}

// UsageRow contains grouped request totals. Cost is an integer micro-USD sum.
type UsageRow struct {
	Key              string `json:"key"`
	Requests         int64  `json:"requests"`
	Errors           int64  `json:"errors"`
	CacheHits        int64  `json:"cache_hits"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	CostMicroUSD     int64  `json:"cost_micro_usd"`
	CostUSD          string `json:"cost_usd"`
	EstimatedRows    int64  `json:"estimated_rows"`
	UnknownCostRows  int64  `json:"unknown_cost_rows"`
}

// CostQuality summarizes non-exact request costs in a time period.
type CostQuality struct {
	EstimatedRows int64 `json:"estimated_rows"`
	UnknownRows   int64 `json:"unknown_rows"`
}

// CostQuality counts requests whose spend includes estimated or unknown cost.
func (s *Store) CostQuality(ctx context.Context, since time.Time) (CostQuality, error) {
	var result CostQuality
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(usage_source = 'estimated'), 0),
		COALESCE(SUM(EXISTS (SELECT 1 FROM attempts WHERE attempts.request_id = requests.id AND attempts.cost_source = 'unknown')), 0)
		FROM requests WHERE at_ns >= ?`, since.UTC().UnixNano()).Scan(&result.EstimatedRows, &result.UnknownRows)
	if err != nil {
		return CostQuality{}, fmt.Errorf("query usage cost quality: %w", err)
	}
	return result, nil
}

// Usage returns grouped request metadata in [Since, Until).
func (s *Store) Usage(ctx context.Context, q UsageQuery) (result []UsageRow, retErr error) {
	group := map[string]string{
		"provider": "provider", "model": "model", "client_app": "client_app",
		"day": "strftime('%Y-%m-%d', at_ns / 1000000000, 'unixepoch')",
	}[q.Group]
	if group == "" {
		return nil, fmt.Errorf("unsupported usage group %q", q.Group)
	}
	query := `SELECT ` + group + `, COUNT(*), SUM(status >= 400), SUM(cache = 'hit'),
		SUM(prompt_tokens), SUM(completion_tokens), SUM(cost_micro_usd),
	SUM(usage_source = 'estimated'),
	SUM(EXISTS (SELECT 1 FROM attempts WHERE attempts.request_id = requests.id AND attempts.cost_source = 'unknown'))
		FROM requests WHERE at_ns >= ? AND at_ns < ? GROUP BY 1 ORDER BY 1`
	rows, err := s.db.QueryContext(ctx, query, q.Since.UTC().UnixNano(), q.Until.UTC().UnixNano())
	if err != nil {
		return nil, fmt.Errorf("query usage totals: %w", err)
	}
	result = make([]UsageRow, 0)
	defer func() {
		if err := rows.Close(); err != nil && retErr == nil {
			retErr = fmt.Errorf("close usage totals query: %w", err)
		}
	}()
	for rows.Next() {
		var row UsageRow
		if err := rows.Scan(&row.Key, &row.Requests, &row.Errors, &row.CacheHits, &row.PromptTokens,
			&row.CompletionTokens, &row.CostMicroUSD, &row.EstimatedRows, &row.UnknownCostRows); err != nil {
			return nil, fmt.Errorf("read usage totals: %w", err)
		}
		row.CostUSD = formatMicroUSD(row.CostMicroUSD)
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read usage totals: %w", err)
	}
	return result, nil
}

// RecentQuery selects a page of requests. Before is the opaque cursor returned
// by the preceding page and is independent of later inserts.
type RecentQuery struct {
	Limit  int
	Before string
}

// Recent returns requests newest first and the cursor for the next page.
func (s *Store) Recent(ctx context.Context, q RecentQuery) (requests []Request, next string, retErr error) {
	if q.Limit < 1 || q.Limit > 200 {
		return nil, "", fmt.Errorf("recent limit must be from 1 to 200")
	}
	type cursor struct {
		MaxID int64 `json:"m"`
		At    int64 `json:"a"`
		ID    int64 `json:"i"`
	}
	var page cursor
	if q.Before != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(q.Before)
		if err != nil {
			return nil, "", ErrInvalidCursor
		}
		if err := json.Unmarshal(decoded, &page); err != nil || page.MaxID < 1 || page.ID < 1 {
			return nil, "", ErrInvalidCursor
		}
	} else if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM requests`).Scan(&page.MaxID); err != nil {
		return nil, "", fmt.Errorf("read recent request boundary: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, at_ns, client_app, requested_model, route, tier, provider, model,
		status, error_kind, stream, cache, latency_ms, prompt_tokens, completion_tokens, usage_source,
		cost_micro_usd, cost_source, skips_json FROM requests
		WHERE id <= ? AND (? = 0 OR at_ns < ? OR (at_ns = ? AND id < ?))
		ORDER BY at_ns DESC, id DESC LIMIT ?`, page.MaxID, page.ID, page.At, page.At, page.ID, q.Limit+1)
	if err != nil {
		return nil, "", fmt.Errorf("query recent requests: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil && retErr == nil {
			retErr = fmt.Errorf("close recent request query: %w", err)
		}
	}()
	var ids []int64
	var timestamps []int64
	for rows.Next() {
		var id, at int64
		var req Request
		var skips string
		if err := rows.Scan(&id, &at, &req.ClientApp, &req.RequestedModel, &req.Route, &req.Tier, &req.Provider, &req.Model,
			&req.Status, &req.ErrorKind, &req.Stream, &req.Cache, &req.LatencyMS, &req.PromptTokens, &req.CompletionTokens,
			&req.UsageSource, &req.CostMicroUSD, &req.CostSource, &skips); err != nil {
			return nil, "", fmt.Errorf("read recent requests: %w", err)
		}
		req.At = time.Unix(0, at).UTC()
		if err := json.Unmarshal([]byte(skips), &req.Skips); err != nil {
			return nil, "", fmt.Errorf("decode recent request skips: %w", err)
		}
		requests = append(requests, req)
		ids = append(ids, id)
		timestamps = append(timestamps, at)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("read recent requests: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, "", fmt.Errorf("close recent request query: %w", err)
	}
	more := len(requests) > q.Limit
	if more {
		requests = requests[:q.Limit]
		ids = ids[:q.Limit]
		timestamps = timestamps[:q.Limit]
	}
	attempts, err := s.requestAttempts(ctx, ids)
	if err != nil {
		return nil, "", err
	}
	for i, id := range ids {
		requests[i].Attempts = attempts[id]
		if requests[i].Attempts == nil {
			requests[i].Attempts = make([]Attempt, 0)
		}
	}
	if more && len(ids) > 0 {
		page.At, page.ID = timestamps[len(timestamps)-1], ids[len(ids)-1]
		data, err := json.Marshal(page)
		if err != nil {
			return nil, "", fmt.Errorf("encode recent request cursor: %w", err)
		}
		next = base64.RawURLEncoding.EncodeToString(data)
	}
	return requests, next, nil
}

func (s *Store) requestAttempts(ctx context.Context, requestIDs []int64) (attempts map[int64][]Attempt, retErr error) {
	attempts = make(map[int64][]Attempt, len(requestIDs))
	if len(requestIDs) == 0 {
		return attempts, nil
	}
	idsJSON, err := json.Marshal(requestIDs)
	if err != nil {
		return nil, fmt.Errorf("encode recent request ids: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT request_id, at_ns, provider, model, outcome, latency_ms, prompt_tokens,
		completion_tokens, usage_source, cost_micro_usd, cost_source FROM attempts
		WHERE request_id IN (SELECT CAST(value AS INTEGER) FROM json_each(?)) ORDER BY request_id, id`, string(idsJSON))
	if err != nil {
		return nil, fmt.Errorf("query request attempts: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil && retErr == nil {
			retErr = fmt.Errorf("close request attempts query: %w", err)
		}
	}()
	for rows.Next() {
		var requestID int64
		var a Attempt
		var at int64
		if err := rows.Scan(&requestID, &at, &a.Provider, &a.Model, &a.Outcome, &a.LatencyMS, &a.PromptTokens,
			&a.CompletionTokens, &a.UsageSource, &a.CostMicroUSD, &a.CostSource); err != nil {
			return nil, fmt.Errorf("read request attempts: %w", err)
		}
		a.At = time.Unix(0, at).UTC()
		attempts[requestID] = append(attempts[requestID], a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read request attempts: %w", err)
	}
	return attempts, nil
}

func formatMicroUSD(micro int64) string {
	whole, fraction := micro/1_000_000, micro%1_000_000
	if fraction < 0 {
		fraction = -fraction
	}
	return fmt.Sprintf("%d.%06d", whole, fraction)
}

var _ interface {
	Usage(context.Context, UsageQuery) ([]UsageRow, error)
	Recent(context.Context, RecentQuery) ([]Request, string, error)
} = (*Store)(nil)
