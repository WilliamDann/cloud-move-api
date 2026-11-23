package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type QueryRequest struct {
	Query      string `json:"query"` // cql source
	PGN        string `json:"pgn"`   // inline corpus
	MaxMatches int    `json:"max_matches"`
}

type Match struct {
	Game  int    `json:"game"`           // 0-based index into the input pgn
	Ply   int    `json:"ply"`            // half-move number of the matched position (0 = start)
	FEN   string `json:"fen,omitempty"`  // the matched position, omitted if replay failed
	Move  string `json:"move,omitempty"` // san leading into the position ("" at the start)
	White string `json:"white,omitempty"`
	Black string `json:"black,omitempty"`
	Date  string `json:"date,omitempty"`
}

type QueryResponse struct {
	Matches      []Match `json:"matches"`
	TotalMatches int     `json:"total_matches"`
	Truncated    bool    `json:"truncated"` // total_matches > len(matches)
	ScannedGames int     `json:"scanned_games"`
}

const (
	maxQueryBytes = 4096
	maxPGNBytes   = 5 << 20 // 5 MB
	maxGames      = 20000
	maxMatchesCap = 100
	defaultTopN   = 25

	// generous ceiling: a 5 MB corpus scan fits comfortably; the hard kill
	// is for pathological queries, not the expected path
	scanTimeout = 120 * time.Second

	// the comment cql inserts at every matched position (-matchstring);
	// distinctive enough to never collide with game annotations
	matchMarker = "cqlmatch"
)

func cqlPath() string {
	if p := os.Getenv("CQL_PATH"); p != "" {
		return p
	}
	return "cql"
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Content-Type", "application/json")
	fmt.Fprint(w, "{ \"state\" : \"ok\" } ")
}

func handleQuery(w http.ResponseWriter, r *http.Request) {
	// bound the body before decoding: query + pgn + json overhead
	r.Body = http.MaxBytesReader(w, r.Body, maxQueryBytes+maxPGNBytes+(1<<20))

	var req QueryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, "invalid request data (body over %d bytes or malformed json)", maxPGNBytes)
		return
	}
	if req.Query == "" {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, "missing query")
		return
	}
	if len(req.Query) > maxQueryBytes {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, "query too long (max %d bytes)", maxQueryBytes)
		return
	}
	if req.PGN == "" {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, "missing pgn")
		return
	}
	if len(req.PGN) > maxPGNBytes {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, "pgn too large (max %d bytes)", maxPGNBytes)
		return
	}
	if games := countGames(req.PGN); games == 0 {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, "pgn contains no games")
		return
	} else if games > maxGames {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, "too many games (max %d)", maxGames)
		return
	}

	topN := req.MaxMatches
	if topN == 0 {
		topN = defaultTopN
	}
	topN = min(topN, maxMatchesCap)

	ctx, cancel := context.WithTimeout(r.Context(), scanTimeout)
	defer cancel()

	result, err := runQuery(ctx, req, topN)
	if err != nil {
		if ctx.Err() != nil {
			w.WriteHeader(http.StatusGatewayTimeout)
			fmt.Fprint(w, "query timed out")
			return
		}
		slog.Error("query failed", "error", err)
		// cql exits nonzero whenever it rejects the query or the pgn —
		// the caller's bug, surfaced as a 400 with cql's diagnostic
		var ie inputError
		if errors.As(err, &ie) {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, ie.msg)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "query failed: %s", err)
		return
	}

	w.Header().Add("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// inputError is a rejection caused by the request's query or pgn — cql
// ran and said no. the message is cql's diagnostic, fit for the client.
type inputError struct{ msg string }

func (e inputError) Error() string { return e.msg }

// runQuery executes one cql invocation in a fresh temp dir and parses the
// output pgn into matches. topN bounds the returned matches, not the scan.
func runQuery(ctx context.Context, req QueryRequest, topN int) (*QueryResponse, error) {
	dir, err := os.MkdirTemp("", "cql-run-*")
	if err != nil {
		return nil, fmt.Errorf("temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	if err := os.WriteFile(filepath.Join(dir, "query.cql"), []byte(req.Query), 0o600); err != nil {
		return nil, fmt.Errorf("write query: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "input.pgn"), []byte(req.PGN), 0o600); err != nil {
		return nil, fmt.Errorf("write pgn: %w", err)
	}

	// -s (single-threaded): multithreaded mode is only faster on big
	// corpora and is less robust on hand-made pgn files, which is exactly
	// what user uploads are. -noheader drops the per-game "Game number N
	// cqlmatch" comment, which contains the matchstring and would otherwise
	// masquerade as a ply-0 position match. -i/-o on the command line
	// override any `input`/`output` the query header tries to set.
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, cqlPath(),
		"-s", "-noheader", "-i", "input.pgn", "-o", "out.pgn", "-matchstring", matchMarker, "query.cql")
	cmd.Dir = dir
	// cql writes even its syntax errors to stdout, not stderr — capture both
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// a killed cql's children (there are none today, but shells wrap it in
	// tests) can hold the stderr pipe open — don't wait on them forever
	cmd.WaitDelay = 3 * time.Second

	runErr := cmd.Run()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("cql timed out")
	}

	out, parseErr := parseOutputPGN(filepath.Join(dir, "out.pgn"), topN)
	if parseErr != nil {
		if runErr != nil {
			if _, ok := runErr.(*exec.ExitError); ok {
				// cql ran and rejected the input — its diagnostic is the
				// error message
				return nil, inputError{msg: diagnostics(stdout, stderr)}
			}
			return nil, fmt.Errorf("cql failed: %w", runErr)
		}
		return nil, fmt.Errorf("parsing cql output: %w", parseErr)
	}
	if runErr != nil {
		// cql exited nonzero but produced parseable output — odd, but the
		// matches are real; log and serve them
		slog.Warn("cql exit status with output", "error", runErr, "output", diagnostics(stdout, stderr))
	}
	out.ScannedGames = countGames(req.PGN)
	return out, nil
}

// diagnostics returns a short snippet of cql's run output for error
// messages and logs. the version banner and "internal error" framing are
// noise — the useful part is the actual complaint with its position info.
func diagnostics(stdout, stderr bytes.Buffer) string {
	var keep []string
	for _, line := range strings.Split(stdout.String()+"\n"+stderr.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "CQL version") {
			continue
		}
		line = strings.ReplaceAll(line, "CQL internal error: ", "")
		keep = append(keep, line)
	}
	msg := strings.Join(keep, " ")
	msg = strings.Join(strings.Fields(msg), " ")
	if len(msg) > 512 {
		msg = msg[len(msg)-512:] // the complaint is at the end of cql's output
	}
	return msg
}

func main() {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", handleHealth)
	mux.HandleFunc("POST /query", handleQuery)

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
