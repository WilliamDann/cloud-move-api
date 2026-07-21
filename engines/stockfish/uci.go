package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type Score struct {
	Type  string `json:"type"` // "cp" (centipawns) or "mate" (moves until mate)
	Value int    `json:"value"`
}

type Line struct {
	Move  string   `json:"move"`
	Score Score    `json:"score"`
	PV    []string `json:"pv"`
}

type AnalysisResponse struct {
	BestMove string `json:"bestmove"`
	Lines    []Line `json:"lines"`
}

const (
	maxDepth     = 30
	maxTime      = 30 // seconds
	maxTopN      = 10
	defaultDepth = 15
	hashMB       = 256
)

func clamp(v, lo, hi int) int {
	return min(max(v, lo), hi)
}

// analyze runs one stockfish process for the request and parses its output.
// Scores are from the side-to-move's perspective, as UCI reports them.
func analyze(ctx context.Context, req AnalysisRequest) (*AnalysisResponse, error) {
	// the handler validates the FEN, but never let a raw string smuggle
	// extra UCI commands into the engine
	if strings.ContainsAny(req.Position, "\r\n") {
		return nil, fmt.Errorf("invalid position")
	}

	moveTimeMs := clamp(req.MovetimeMs, 0, maxTime*1000)
	if moveTimeMs == 0 {
		moveTimeMs = clamp(req.Time, 0, maxTime) * 1000
	}
	depth := req.Depth
	if depth == 0 && moveTimeMs == 0 {
		depth = defaultDepth
	}
	if depth != 0 {
		depth = clamp(depth, 1, maxDepth)
	}
	topN := clamp(req.TopN, 1, maxTopN)

	// hard ceiling so a wedged engine can't hold a request forever
	ctx, cancel := context.WithTimeout(ctx, (maxTime+30)*time.Second)
	defer cancel()

	enginePath := os.Getenv("STOCKFISH_PATH")
	if enginePath == "" {
		enginePath = "stockfish"
	}

	cmd := exec.CommandContext(ctx, enginePath)
	cmd.WaitDelay = 3 * time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	defer cmd.Wait()
	defer stdin.Close()

	// write errors are sticky; they surface at the next waitFor
	var sendErr error
	send := func(format string, args ...any) {
		if sendErr == nil {
			_, sendErr = fmt.Fprintf(stdin, format+"\n", args...)
		}
	}

	scanner := bufio.NewScanner(stdout)
	waitFor := func(token string) error {
		for scanner.Scan() {
			if strings.TrimSpace(scanner.Text()) == token {
				return nil
			}
		}
		if sendErr != nil {
			return sendErr
		}
		if err := scanner.Err(); err != nil {
			return err
		}
		return fmt.Errorf("engine closed before %q", token)
	}

	send("uci")
	if err := waitFor("uciok"); err != nil {
		return nil, err
	}
	send("setoption name Threads value %d", runtime.GOMAXPROCS(0))
	// stockfish defaults to a 16MB hash; the instance has memory to spare,
	// and a bigger table meaningfully speeds depth ~20 searches
	send("setoption name Hash value %d", hashMB)
	if req.Skill != nil {
		send("setoption name Skill Level value %d", clamp(*req.Skill, 0, 20))
	}
	send("setoption name MultiPV value %d", topN)
	posCmd := "position startpos"
	if req.Position != "" {
		posCmd = "position fen " + req.Position
	}
	if len(req.Moves) > 0 {
		posCmd += " moves " + strings.Join(req.Moves, " ")
	}
	send("%s", posCmd)
	send("isready")
	if err := waitFor("readyok"); err != nil {
		return nil, err
	}

	goCmd := "go"
	if depth != 0 {
		goCmd += " depth " + strconv.Itoa(depth)
	}
	if moveTimeMs != 0 {
		goCmd += " movetime " + strconv.Itoa(moveTimeMs)
	}
	send("%s", goCmd)

	lines := map[int]Line{}
	bestMove := ""
	for scanner.Scan() {
		text := scanner.Text()
		if strings.HasPrefix(text, "bestmove") {
			if fields := strings.Fields(text); len(fields) >= 2 {
				bestMove = fields[1]
			}
			break
		}
		if line, rank, ok := parseInfo(text); ok {
			lines[rank] = line
		}
	}
	send("quit")

	if bestMove == "" {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := scanner.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("engine closed before bestmove")
	}

	resp := &AnalysisResponse{BestMove: bestMove}
	for rank := 1; rank <= topN; rank++ {
		if line, ok := lines[rank]; ok {
			resp.Lines = append(resp.Lines, line)
		}
	}
	return resp, nil
}

// parseInfo pulls rank, score and pv out of one "info ..." search line.
// Lines without a pv, or with aspiration-window bound scores, are skipped.
func parseInfo(text string) (Line, int, bool) {
	fields := strings.Fields(text)
	if len(fields) == 0 || fields[0] != "info" {
		return Line{}, 0, false
	}

	line := Line{}
	rank := 1
	for i := 1; i < len(fields); i++ {
		switch fields[i] {
		case "multipv":
			if i+1 < len(fields) {
				rank, _ = strconv.Atoi(fields[i+1])
				i++
			}
		case "score":
			if i+2 < len(fields) {
				line.Score.Type = fields[i+1]
				line.Score.Value, _ = strconv.Atoi(fields[i+2])
				i += 2
			}
		case "lowerbound", "upperbound":
			return Line{}, 0, false
		case "pv":
			line.PV = fields[i+1:]
			i = len(fields)
		}
	}

	if len(line.PV) == 0 || line.Score.Type == "" {
		return Line{}, 0, false
	}
	line.Move = line.PV[0]
	return line, rank, true
}
