package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"

	"github.com/notnil/chess"
)

type AnalysisRequest struct {
	Position    string   `json:"position"`    // base FEN; standard start position if empty
	Moves       []string `json:"moves"`       // UCI moves from position (enables history conditioning)
	Depth       int      `json:"depth"`       // accepted for contract parity, ignored by maia
	Time        int      `json:"time"`        // accepted for contract parity, ignored by maia
	TopN        int      `json:"top_n"`       // top n lines
	Elo         int      `json:"elo"`         // skill level to imitate, engine default if 0
	Temperature *float64 `json:"temperature"` // policy sampling temperature; 0 = argmax
	Model       string   `json:"model"`       // maia3-5m (default) | maia3-23m | maia3-79m
}

const maxMoves = 600

// validateGame checks the FEN parses and every move is legal from it, so a
// raw string can never smuggle UCI commands into the engine
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

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Content-Type", "application/json")
	fmt.Fprint(w, "{ \"state\" : \"ok\" } ")
}

func handleAnalysis(w http.ResponseWriter, r *http.Request) {
	var analysisRequest AnalysisRequest
	var err error

	// check request information
	err = json.NewDecoder(r.Body).Decode(&analysisRequest)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, "invalid request data")
		return
	}

	if analysisRequest.Model == "" {
		analysisRequest.Model = defaultModel
	}
	if !knownModels[analysisRequest.Model] {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, "unknown model")
		return
	}

	err = validateGame(analysisRequest.Position, analysisRequest.Moves)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, err.Error())
		return
	}

	result, err := analyze(r.Context(), analysisRequest)
	if err != nil {
		slog.Error("analysis failed", "model", analysisRequest.Model, "error", err)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "analysis failed")
		return
	}

	w.Header().Add("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// handleWarm pre-loads a model so the first analysis skips the cold start.
// It blocks until the engine is up: cloud run only guarantees cpu while a
// request is in flight, so callers should fire-and-forget rather than await.
func handleWarm(w http.ResponseWriter, r *http.Request) {
	var warmRequest struct {
		Model string `json:"model"`
	}
	err := json.NewDecoder(r.Body).Decode(&warmRequest)
	if err != nil && !errors.Is(err, io.EOF) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, "invalid request data")
		return
	}

	if warmRequest.Model == "" {
		warmRequest.Model = defaultModel
	}
	if !knownModels[warmRequest.Model] {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, "unknown model")
		return
	}

	alreadyWarm := isWarm(warmRequest.Model)
	// the weights load lazily on the first search, so spawning the process
	// alone isn't enough — run a throwaway search to force the load
	_, err = analyze(r.Context(), AnalysisRequest{Model: warmRequest.Model})
	if err != nil {
		slog.Error("warm failed", "model", warmRequest.Model, "error", err)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "warm failed")
		return
	}

	w.Header().Add("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"model": warmRequest.Model, "already_warm": alreadyWarm})
}

func main() {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", handleHealth)
	mux.HandleFunc("POST /analysis", handleAnalysis)
	mux.HandleFunc("POST /warm", handleWarm)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	slog.Info("started http server", "addr", "0.0.0.0:"+port)
	err := http.ListenAndServe("0.0.0.0:"+port, mux)
	if err != nil {
		slog.Error("server failed", "error", err)
		os.Exit(1)
	}
}
