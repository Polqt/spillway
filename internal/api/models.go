package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/Polqt/spillway/internal/providers"
)

type modelsHandler struct {
	providers providerSet
	logger    *slog.Logger
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
	data := make([]modelItem, 0)
	failed := 0
	for _, provider := range configured {
		models, err := provider.Models(r.Context())
		if err != nil {
			failed++
			h.logger.WarnContext(r.Context(), "provider model listing failed", "provider", provider.Name())
			continue
		}
		for _, model := range models {
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
	if failed == len(configured) {
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
