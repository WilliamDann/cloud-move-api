package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/notnil/chess"
)

type PredictRequest struct {
	ModelID     string   `json:"model_id"`
	Position    string   `json:"position"`    // base FEN; standard start position if empty
	Moves       []string `json:"moves"`       // UCI moves from position (lets maia condition on the line played)
	Depth       int      `json:"depth"`       // capped
	Time        int      `json:"time"`        // capped, seconds
	MovetimeMs  int      `json:"movetime_ms"` // capped; overrides time, stockfish only
	TopN        int      `json:"top_n"`       // capped, default 1
	Elo         int      `json:"elo"`         // overrides the model's configured default
	Temperature *float64 `json:"temperature"` // maia policy sampling; 0 = argmax
	Skill       *int     `json:"skill"`       // stockfish Skill Level 0-20; omit for full strength
}

type Score struct {
	Type  string `json:"type"` // "cp" (centipawns) or "mate" (moves until mate)
	Value int    `json:"value"`
}

type Line struct {
	Move  string   `json:"move"`
	Score Score    `json:"score"`
	PV    []string `json:"pv"`
	WDL   []int    `json:"wdl,omitempty"` // win/draw/loss per mille, side to move
}

type PredictResponse struct {
	Move  string   `json:"move"`
	Eval  Score    `json:"eval"`
	PV    []string `json:"pv"`
	WDL   []int    `json:"wdl,omitempty"`
	Lines []Line   `json:"lines"`
}

type engineRequest struct {
	Position    string   `json:"position,omitempty"`
	Moves       []string `json:"moves,omitempty"`
	Depth       int      `json:"depth,omitempty"`
	Time        int      `json:"time,omitempty"`
	MovetimeMs  int      `json:"movetime_ms,omitempty"`
	TopN        int      `json:"top_n,omitempty"`
	Elo         int      `json:"elo,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
	Model       string   `json:"model,omitempty"`
	Skill       *int     `json:"skill,omitempty"`
}

type engineResponse struct {
	BestMove string `json:"bestmove"`
	Lines    []Line `json:"lines"`
}

const (
	maxDepth = 30
	maxTime  = 30 // seconds
	maxTopN  = 10
	maxMoves = 600

	// ~1KB per entry; comfortably holds every position of a day's lessons
	maxCacheEntries = 4096

	// /query limits — mirror engines/cql; the gateway enforces them first
	// so bad requests never reach the engine
	maxQueryBytes = 4096
	maxPGNBytes   = 5 << 20 // 5 MB
	maxMatchesCap = 100
)

// validateGame checks the FEN parses and every move is legal from it
func validateGame(fen string, moves []string) error {
	game := chess.NewGame()
	if fen != "" {
		base, err := chess.FEN(fen)
		if err != nil {
			return fmt.Errorf("invalid fen")
		}
		game = chess.NewGame(base)
	}
	if len(moves) > maxMoves {
		return fmt.Errorf("too many moves")
	}
	for _, uci := range moves {
		move, err := chess.UCINotation{}.Decode(game.Position(), uci)
		if err != nil {
			return fmt.Errorf("invalid move %q", uci)
		}
		if err := game.Move(move); err != nil {
			return fmt.Errorf("illegal move %q", uci)
		}
	}
	return nil
}

func clamp(v, lo, hi int) int {
	return min(max(v, lo), hi)
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Content-Type", "application/json")
	fmt.Fprint(w, "{ \"state\" : \"ok\" } ")
}

func (reg *registry) handleModels(w http.ResponseWriter, r *http.Request) {
	type modelInfo struct {
		ModelID string `json:"model_id"`
		Name    string `json:"name"`
	}
	infos := make([]modelInfo, 0, len(reg.models))
	for _, model := range reg.models {
		infos = append(infos, modelInfo{ModelID: model.ID, Name: model.Name})
	}

	w.Header().Add("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"models": infos})
}

func (reg *registry) handlePredict(w http.ResponseWriter, r *http.Request) {
	var predictRequest PredictRequest
	var err error

	// check request information
	err = json.NewDecoder(r.Body).Decode(&predictRequest)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, "invalid request data")
		return
	}

	model, ok := reg.byID[predictRequest.ModelID]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, "unknown model")
		return
	}

	err = validateGame(predictRequest.Position, predictRequest.Moves)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, err.Error())
		return
	}

	elo := predictRequest.Elo
	if elo == 0 {
		elo = model.Elo
	}

	body, err := json.Marshal(engineRequest{
		Position:    predictRequest.Position,
		Moves:       predictRequest.Moves,
		Depth:       clamp(predictRequest.Depth, 0, maxDepth),
		Time:        clamp(predictRequest.Time, 0, maxTime),
		MovetimeMs:  clamp(predictRequest.MovetimeMs, 0, maxTime*1000),
		TopN:        clamp(predictRequest.TopN, 1, maxTopN),
		Elo:         elo,
		Temperature: predictRequest.Temperature,
		Model:       model.Model,
		Skill:       predictRequest.Skill,
	})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "prediction failed")
		return
	}

	var engineResp engineResponse
	// skill-limited play is randomized by design (weak bots vary their
	// mistakes), so those responses must never be cached
	if model.Cache && predictRequest.Skill == nil {
		// the marshaled body is canonical (fixed field order), so it is the key
		var hit bool
		engineResp, hit, err = reg.cache.do(model.ID+"|"+string(body), func() (engineResponse, error) {
			return reg.callEngine(r.Context(), model, body)
		})
		if err == nil {
			if hit {
				w.Header().Set("X-Cache", "HIT")
			} else {
				w.Header().Set("X-Cache", "MISS")
			}
		}
	} else {
		engineResp, err = reg.callEngine(r.Context(), model, body)
	}
	if err != nil {
		slog.Error("engine request failed", "model", model.ID, "error", err)
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, "engine error")
		return
	}

	predictResponse := PredictResponse{Move: engineResp.BestMove, Lines: engineResp.Lines}
	// eval, pv and wdl come from the line matching the chosen move; maia can
	// sample a move that isn't the top-ranked line
	for _, line := range engineResp.Lines {
		if line.Move == predictResponse.Move {
			predictResponse.Eval = line.Score
			predictResponse.PV = line.PV
			predictResponse.WDL = line.WDL
			break
		}
	}
	if predictResponse.PV == nil && len(engineResp.Lines) > 0 {
		// no reported line for the sampled move: eval and wdl are the
		// engine's top assessment, and the pv can only claim the move itself
		predictResponse.Eval = engineResp.Lines[0].Score
		predictResponse.WDL = engineResp.Lines[0].WDL
		predictResponse.PV = []string{predictResponse.Move}
	}

	w.Header().Add("Content-Type", "application/json")
	json.NewEncoder(w).Encode(predictResponse)
}

// callEngine posts one analysis request to the model's engine api
func (reg *registry) callEngine(ctx context.Context, model Model, body []byte) (engineResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, model.URL+"/analysis", bytes.NewReader(body))
	if err != nil {
		return engineResponse{}, fmt.Errorf("engine request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := reg.clients[model.URL].Do(req)
	if err != nil {
		return engineResponse{}, fmt.Errorf("engine unavailable: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return engineResponse{}, fmt.Errorf("engine status %d: %s", resp.StatusCode, msg)
	}

	var engineResp engineResponse
	if err := json.NewDecoder(resp.Body).Decode(&engineResp); err != nil {
		return engineResponse{}, fmt.Errorf("invalid engine response: %w", err)
	}
	return engineResp, nil
}

// handleWarm asks a model's engine to pre-load so the first predict of a
// game skips the cold start. It blocks until the engine is ready — callers
// should fire-and-forget rather than await it.
func (reg *registry) handleWarm(w http.ResponseWriter, r *http.Request) {
	var warmRequest struct {
		ModelID string `json:"model_id"`
	}
	err := json.NewDecoder(r.Body).Decode(&warmRequest)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, "invalid request data")
		return
	}

	model, ok := reg.byID[warmRequest.ModelID]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, "unknown model")
		return
	}

	body, err := json.Marshal(map[string]string{"model": model.Model})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "warm failed")
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, model.URL+"/warm", bytes.NewReader(body))
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "warm failed")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := reg.clients[model.URL].Do(req)
	if err != nil {
		slog.Error("engine warm failed", "model", model.ID, "error", err)
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, "engine unavailable")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		slog.Error("engine warm returned error", "model", model.ID, "status", resp.StatusCode, "body", string(msg))
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, "engine error")
		return
	}

	var warmResp struct {
		AlreadyWarm bool `json:"already_warm"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&warmResp); err != nil {
		slog.Error("invalid engine warm response", "model", model.ID, "error", err)
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, "engine error")
		return
	}

	w.Header().Add("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"model_id": model.ID, "already_warm": warmResp.AlreadyWarm})
}

// handleQuery forwards a CQL query + pgn corpus to the cql engine. The
// response is streamed through unchanged — the engine owns the schema.
func (reg *registry) handleQuery(w http.ResponseWriter, r *http.Request) {
	if reg.cqlURL == "" {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, "cql engine not configured")
		return
	}

	// bound the body before decoding: query + pgn + json overhead
	r.Body = http.MaxBytesReader(w, r.Body, maxQueryBytes+maxPGNBytes+(1<<20))

	var req struct {
		Query      string `json:"query"`
		PGN        string `json:"pgn"`
		MaxMatches *int   `json:"max_matches"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, "invalid request data (body over 5 MB or malformed json)")
		return
	}
	if req.Query == "" || req.PGN == "" {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, "missing query or pgn")
		return
	}
	if len(req.Query) > maxQueryBytes {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, "query too long (max %d bytes)", maxQueryBytes)
		return
	}
	if len(req.PGN) > maxPGNBytes {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, "pgn too large (max %d bytes)", maxPGNBytes)
		return
	}
	topN := 25
	if req.MaxMatches != nil {
		topN = *req.MaxMatches
	}
	topN = clamp(topN, 1, maxMatchesCap)

	body, err := json.Marshal(map[string]any{"query": req.Query, "pgn": req.PGN, "max_matches": topN})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "query failed")
		return
	}

	req2, err := http.NewRequestWithContext(r.Context(), http.MethodPost, reg.cqlURL+"/query", bytes.NewReader(body))
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "query failed")
		return
	}
	req2.Header.Set("Content-Type", "application/json")
	resp, err := reg.clients[reg.cqlURL].Do(req2)
	if err != nil {
		slog.Error("cql engine unavailable", "error", err)
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, "engine unavailable")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
		return
	}
	// pass engine error status and a short body through
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	w.WriteHeader(resp.StatusCode)
	w.Write(msg)
}

// cors allows the browser app to call the gateway from another origin
func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-API-Key")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// apiKey rejects requests that don't present a configured key in X-API-Key.
// /health stays open for uptime probes.
func apiKey(keys []string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}
		got := []byte(r.Header.Get("X-API-Key"))
		for _, key := range keys {
			if subtle.ConstantTimeCompare(got, []byte(key)) == 1 {
				next.ServeHTTP(w, r)
				return
			}
		}
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, "invalid api key")
	})
}

func main() {
	reg, err := newRegistry(context.Background())
	if err != nil {
		slog.Error("loading models", "error", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", handleHealth)
	mux.HandleFunc("GET /models", reg.handleModels)
	mux.HandleFunc("POST /predict", reg.handlePredict)
	mux.HandleFunc("POST /warm", reg.handleWarm)
	mux.HandleFunc("POST /query", reg.handleQuery)

	// comma-separated so a new key can be added before the old one is retired
	var keys []string
	for _, key := range strings.Split(os.Getenv("API_KEYS"), ",") {
		if key = strings.TrimSpace(key); key != "" {
			keys = append(keys, key)
		}
	}
	var handler http.Handler = mux
	if len(keys) == 0 {
		slog.Warn("API_KEYS not set, api key check disabled")
	} else {
		handler = apiKey(keys, handler)
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	slog.Info("started http server", "addr", "0.0.0.0:"+port)
	err = http.ListenAndServe("0.0.0.0:"+port, cors(handler))
	if err != nil {
		slog.Error("server failed", "error", err)
		os.Exit(1)
	}
}
