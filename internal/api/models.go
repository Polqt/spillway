package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Polqt/spillway/internal/providers"
)

type modelsHandler struct {
	providers providerSet
	logger    *slog.Logger
	timeout   time.Duration
	tiers     map[string][]providers.ModelRef
}

type modelList struct {
	Object string      `json:"object"`
	Data   []modelItem `json:"data"`
}

type modelItem struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

func (h *modelsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	configured := h.providers.Providers()
	type result struct {
		models []providers.Model
		err    error
	}
	results := make([]result, len(configured))
	var wg sync.WaitGroup
	for i, provider := range configured {
		wg.Add(1)
		go func(ctx context.Context, i int, provider providers.Provider) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(ctx, h.timeout)
			defer cancel()
			results[i].models, results[i].err = provider.Models(ctx)
		}(r.Context(), i, provider)
	}
	wg.Wait()

	data := make([]modelItem, 0)
	failed := 0
	for i, provider := range configured {
		if results[i].err != nil {
			failed++
			h.logger.WarnContext(r.Context(), "provider model listing failed", "provider", provider.Name())
			continue
		}
		for _, model := range results[i].models {
			ownedBy := model.OwnedBy
			if ownedBy == "" {
				ownedBy = provider.Name()
			}
			data = append(data, modelItem{
				ID:      providers.ModelRef{Provider: provider.Name(), Model: model.ID}.String(),
				Object:  "model",
				Created: model.Created,
				OwnedBy: ownedBy,
			})
		}
	}
	for name := range h.tiers {
		data = append(data, modelItem{ID: "spillway/" + name, Object: "model", OwnedBy: "spillway"})
	}
	if failed == len(configured) && len(h.tiers) == 0 {
		writeError(w, http.StatusBadGateway, "All configured providers failed to list models; retry later.", typeAPI, "upstream_unavailable")
		return
	}
	slices.SortFunc(data, func(a, b modelItem) int {
		if order := strings.Compare(a.ID, b.ID); order != 0 {
			return order
		}
		if order := strings.Compare(a.OwnedBy, b.OwnedBy); order != 0 {
			return order
		}
		if a.Created < b.Created {
			return -1
		}
		if a.Created > b.Created {
			return 1
		}
		return 0
	})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(modelList{Object: "list", Data: data})
}
