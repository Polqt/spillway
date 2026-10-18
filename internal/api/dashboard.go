package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Polqt/spillway/internal/budget"
	"github.com/Polqt/spillway/internal/quota"
	"github.com/Polqt/spillway/internal/store"
)

func noStoreAPI(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.URL.Path) >= 4 && r.URL.Path[:4] == "/api" {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func serveUsage(w http.ResponseWriter, r *http.Request, o Options) {
	if o.DashboardStore == nil {
		writeError(w, http.StatusServiceUnavailable, "Usage storage is not configured.", typeAPI, "store_disabled")
		return
	}
	now := o.Now()
	until, err := parseTimeParam(r, "until", now)
	if err != nil {
		writeQueryError(w, err)
		return
	}
	since, err := parseTimeParam(r, "since", until.Add(-30*24*time.Hour))
	if err != nil {
		writeQueryError(w, err)
		return
	}
	if !since.Before(until) || until.Sub(since) > 400*24*time.Hour {
		writeError(w, http.StatusBadRequest, "since and until must describe a positive range of at most 400 days.", typeInvalidRequest, "invalid_query")
		return
	}
	group := r.URL.Query().Get("group")
	if group == "" {
		group = "provider"
	}
	switch group {
	case "provider", "model", "client_app", "day":
	default:
		writeError(w, http.StatusBadRequest, "group must be provider, model, client_app or day.", typeInvalidRequest, "invalid_query")
		return
	}
	rows, err := o.DashboardStore.Usage(r.Context(), store.UsageQuery{Since: since, Until: until, Group: store.UsageGroup(group)})
	if err != nil {
		writeInternalError(w)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Since string           `json:"since"`
		Until string           `json:"until"`
		Group string           `json:"group"`
		Rows  []store.UsageRow `json:"data"`
	}{since.UTC().Format(time.RFC3339), until.UTC().Format(time.RFC3339), group, rows})
}

func serveDecisions(w http.ResponseWriter, r *http.Request, o Options) {
	if o.DashboardStore == nil {
		writeError(w, http.StatusServiceUnavailable, "Usage storage is not configured.", typeAPI, "store_disabled")
		return
	}
	limit := 50
	if value := r.URL.Query().Get("limit"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 200 {
			writeError(w, http.StatusBadRequest, "limit must be from 1 to 200.", typeInvalidRequest, "invalid_query")
			return
		}
		limit = parsed
	}
	requests, next, err := o.DashboardStore.Recent(r.Context(), store.RecentQuery{Limit: limit, Before: r.URL.Query().Get("before")})
	if err != nil {
		if errors.Is(err, store.ErrInvalidCursor) {
			writeError(w, http.StatusBadRequest, "before must be a valid cursor.", typeInvalidRequest, "invalid_query")
		} else {
			writeInternalError(w)
		}
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Data       []store.Request `json:"data"`
		NextCursor string          `json:"next_cursor,omitempty"`
	}{requests, next})
}

func serveBudget(w http.ResponseWriter, r *http.Request, o Options) {
	status := budget.Status{}
	if o.BudgetGate != nil {
		status = o.BudgetGate.Status()
	}
	result := struct {
		budget.Status
		IncludesEstimatedCosts bool `json:"includes_estimated_costs"`
		IncludesUnknownCosts   bool `json:"includes_unknown_costs"`
	}{Status: status}
	if o.DashboardStore != nil {
		now := o.Now().UTC()
		since := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		if status.Monthly != nil {
			since = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		}
		quality, err := o.DashboardStore.CostQuality(r.Context(), since)
		if err != nil {
			writeInternalError(w)
			return
		}
		result.IncludesEstimatedCosts = quality.EstimatedRows > 0
		result.IncludesUnknownCosts = quality.UnknownRows > 0
	}
	writeJSON(w, http.StatusOK, result)
}

func serveQuotas(w http.ResponseWriter, _ *http.Request, o Options) {
	var candidates any = []any{}
	if o.Tracker != nil {
		candidates = o.Tracker.Snapshot()
	}
	writeJSON(w, http.StatusOK, struct {
		Candidates any `json:"candidates"`
	}{candidates})
}

func serveProviderStatus(w http.ResponseWriter, _ *http.Request, o Options) {
	type signal struct {
		Model            string             `json:"model"`
		Reason           string             `json:"reason"`
		CooldownUntil    *time.Time         `json:"cooldown_until,omitempty"`
		BreakerState     quota.BreakerState `json:"breaker_state,omitempty"`
		UpstreamFailures int                `json:"upstream_failures,omitempty"`
	}
	type providerStatus struct {
		Name               string   `json:"name"`
		Configured         bool     `json:"configured"`
		CredentialRequired bool     `json:"credential_required"`
		CredentialPresent  bool     `json:"credential_present"`
		Signals            []signal `json:"signals"`
	}
	statuses := make([]providerStatus, 0, len(o.DashboardProviders))
	index := make(map[string]int, len(o.DashboardProviders))
	for _, configured := range o.DashboardProviders {
		index[configured.Name] = len(statuses)
		statuses = append(statuses, providerStatus{
			Name: configured.Name, Configured: true,
			CredentialRequired: configured.CredentialRequired,
			CredentialPresent:  configured.CredentialPresent,
			Signals:            []signal{},
		})
	}
	if o.Tracker != nil {
		for _, candidate := range o.Tracker.Snapshot() {
			providerIndex, exists := index[candidate.Provider]
			if !exists || candidate.CooldownUntil == nil && candidate.Breaker != quota.BreakerOpen && candidate.UpstreamFailures == 0 {
				continue
			}
			statuses[providerIndex].Signals = append(statuses[providerIndex].Signals, signal{
				Model: candidate.Model, Reason: candidate.Reason,
				CooldownUntil: candidate.CooldownUntil, BreakerState: candidate.Breaker,
				UpstreamFailures: candidate.UpstreamFailures,
			})
		}
	}
	slices.SortFunc(statuses, func(a, b providerStatus) int { return strings.Compare(a.Name, b.Name) })
	writeJSON(w, http.StatusOK, struct {
		Providers []providerStatus `json:"providers"`
	}{statuses})
}

func parseTimeParam(r *http.Request, name string, fallback time.Time) (time.Time, error) {
	value := r.URL.Query().Get(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, errors.New(name + " must be an RFC3339 timestamp")
	}
	return parsed, nil
}

func writeQueryError(w http.ResponseWriter, err error) {
	writeError(w, http.StatusBadRequest, err.Error()+".", typeInvalidRequest, "invalid_query")
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
