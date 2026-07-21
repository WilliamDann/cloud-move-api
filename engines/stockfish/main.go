package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/notnil/chess"
)

type AnalysisRequest struct {
	Position   string   `json:"position"`    // base FEN; standard start position if empty
	Moves      []string `json:"moves"`       // UCI moves from position
	Depth      int      `json:"depth"`       // depth
	Time       int      `json:"time"`        // search time in seconds
	MovetimeMs int      `json:"movetime_ms"` // search time in ms; overrides time
	TopN       int      `json:"top_n"`       // top n lines
	Skill      *int     `json:"skill"`       // UCI Skill Level 0-20; omit for full strength
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

	err = validateGame(analysisRequest.Position, analysisRequest.Moves)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, err.Error())
		return
	}

	result, err := analyze(r.Context(), analysisRequest)
	if err != nil {
		slog.Error("analysis failed", "error", err)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "analysis failed")
		return
	}

	w.Header().Add("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func main() {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", handleHealth)
	mux.HandleFunc("POST /analysis", handleAnalysis)
	// stockfish starts in milliseconds, so there is nothing to pre-load;
	// the endpoint exists for contract parity with the maia engine
	mux.HandleFunc("POST /warm", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Content-Type", "application/json")
		fmt.Fprint(w, `{"already_warm":true}`)
	})

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
