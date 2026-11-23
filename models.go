package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"google.golang.org/api/idtoken"
)

type Model struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	URL   string `json:"url"`             // internal engine api url
	Elo   int    `json:"elo,omitempty"`   // default elo when the request doesn't set one
	Model string `json:"model,omitempty"` // engine-side model name (e.g. maia3 checkpoint)
	Cache bool   `json:"cache,omitempty"` // memoize responses — deterministic analysis only, never sampled engines
}

// local dev defaults; production sets MODELS to a json array of Model
const defaultModels = `[
	{"id": "stockfish", "name": "Stockfish 17", "url": "http://localhost:8081", "cache": true},
	{"id": "maia3", "name": "Maia 3", "url": "http://localhost:8082", "model": "maia3-79m"},
	{"id": "maia3-fast", "name": "Maia 3 Fast", "url": "http://localhost:8082", "model": "maia3-23m"},
	{"id": "maia3-xfast", "name": "Maia 3 xFast", "url": "http://localhost:8082", "model": "maia3-5m"}
]`

type registry struct {
	models  []Model
	byID    map[string]Model
	clients map[string]*http.Client // one per engine url
	cache   *resultCache            // shared across cache-enabled models
	cqlURL  string                  // cql engine api url ("" = not configured)
}

func newRegistry(ctx context.Context) (*registry, error) {
	raw := os.Getenv("MODELS")
	if raw == "" {
		raw = defaultModels
	}

	var models []Model
	if err := json.Unmarshal([]byte(raw), &models); err != nil {
		return nil, fmt.Errorf("parsing MODELS: %w", err)
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("no models configured")
	}

	reg := &registry{
		byID:    map[string]Model{},
		clients: map[string]*http.Client{},
		cache:   newResultCache(maxCacheEntries),
	}
	for _, model := range models {
		reg.models = append(reg.models, model)
		reg.byID[model.ID] = model

		if _, ok := reg.clients[model.URL]; ok {
			continue
		}
		// on cloud run this attaches an identity token for the engine;
		// locally there is no service account, so fall back to plain http
		// and run the engines unauthenticated
		client, err := idtoken.NewClient(ctx, model.URL)
		if err != nil {
			slog.Warn("using unauthenticated client", "url", model.URL, "error", err)
			client = &http.Client{}
		}
		// generous: a predict or warm that lands on a cold maia model
		// pays the weight load before the search runs
		client.Timeout = 150 * time.Second
		reg.clients[model.URL] = client
	}

	// the cql engine is not a predict model — one url, routed separately
	if cqlURL := os.Getenv("CQL_ENGINE_URL"); cqlURL != "" {
		reg.cqlURL = cqlURL
		if _, ok := reg.clients[cqlURL]; !ok {
			client, err := idtoken.NewClient(ctx, cqlURL)
			if err != nil {
				slog.Warn("using unauthenticated client", "url", cqlURL, "error", err)
				client = &http.Client{}
			}
			client.Timeout = 150 * time.Second
			reg.clients[cqlURL] = client
		}
	}
	return reg, nil
}
