package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
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
	WDL   []int    `json:"wdl,omitempty"` // win/draw/loss per mille, side to move
}

type AnalysisResponse struct {
	BestMove string `json:"bestmove"`
	Lines    []Line `json:"lines"`
}

const (
	maxTopN            = 10
	defaultModel       = "maia3-5m"
	defaultElo         = 1500
	defaultTemperature = 0.2 // policy sampling temperature; 0 = argmax

	// maia3's trained rating range
	minElo = 600
	maxElo = 2600
)

// models baked into the image (see Dockerfile); the handlers validate against
// this so a bad model name can't spawn a doomed engine process
var knownModels = map[string]bool{"maia3-5m": true, "maia3-23m": true, "maia3-79m": true}

// engine is one long-lived maia3 process. Loading the model weights is the
// expensive part (and happens lazily on the first search, not the uci
// handshake), so processes stay warm across requests. The mutex serializes
// the configure+search pair — a UCI process is single-threaded and stateful.
type engine struct {
	mu       sync.Mutex
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	scanner  *bufio.Scanner
	waitOnce sync.Once
}

var registry = struct {
	sync.Mutex
	engines map[string]*engine
}{engines: map[string]*engine{}}

func engineFor(model string) (*engine, error) {
	registry.Lock()
	defer registry.Unlock()
	if eng, ok := registry.engines[model]; ok {
		return eng, nil
	}
	eng, err := spawn(model)
	if err != nil {
		return nil, err
	}
	registry.engines[model] = eng
	return eng, nil
}

func isWarm(model string) bool {
	registry.Lock()
	defer registry.Unlock()
	_, ok := registry.engines[model]
	return ok
}

// drop forgets a wedged engine so the next request respawns it
func drop(model string, eng *engine) {
	eng.kill()
	registry.Lock()
	if registry.engines[model] == eng {
		delete(registry.engines, model)
	}
	registry.Unlock()
}

func spawn(model string) (*engine, error) {
	engineCmd := os.Getenv("ENGINE_CMD")
	if engineCmd == "" {
		engineCmd = "maia3-uci --device cpu --no-use-amp --use-uci-history"
	}
	parts := append(strings.Fields(engineCmd), "--model", model)

	cmd := exec.Command(parts[0], parts[1:]...)
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

	eng := &engine{cmd: cmd, stdin: stdin, scanner: bufio.NewScanner(stdout)}

	// the handshake itself is quick — weights load on the first search
	timer := time.AfterFunc(30*time.Second, eng.kill)
	defer timer.Stop()
	if err := eng.send("uci"); err != nil {
		eng.kill()
		return nil, err
	}
	if err := eng.waitFor("uciok"); err != nil {
		eng.kill()
		return nil, err
	}
	return eng, nil
}

func (e *engine) send(format string, args ...any) error {
	_, err := fmt.Fprintf(e.stdin, format+"\n", args...)
	return err
}

func (e *engine) waitFor(token string) error {
	for e.scanner.Scan() {
		if strings.TrimSpace(e.scanner.Text()) == token {
			return nil
		}
	}
	if err := e.scanner.Err(); err != nil {
		return err
	}
	return fmt.Errorf("engine closed before %q", token)
}

func (e *engine) kill() {
	if e.cmd.Process != nil {
		e.cmd.Process.Kill()
	}
	e.waitOnce.Do(func() { go e.cmd.Wait() })
}

func clamp(v, lo, hi int) int {
	return min(max(v, lo), hi)
}

// analyze runs one search on the model's persistent engine. The handler has
// already validated the model name, position and moves.
func analyze(ctx context.Context, req AnalysisRequest) (*AnalysisResponse, error) {
	topN := clamp(req.TopN, 1, maxTopN)
	elo := req.Elo
	if elo == 0 {
		elo = defaultElo
	}
	elo = clamp(elo, minElo, maxElo)
	temperature := defaultTemperature
	if req.Temperature != nil {
		temperature = min(max(*req.Temperature, 0), 2)
	}

	// generous ceiling: the first search on a model also pays the lazy
	// weight load
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	eng, err := engineFor(req.Model)
	if err != nil {
		return nil, err
	}
	eng.mu.Lock()
	defer eng.mu.Unlock()

	// a shared stateful process can't abandon a search mid-flight; on
	// timeout kill it and let the next request respawn
	stop := context.AfterFunc(ctx, func() { drop(req.Model, eng) })
	defer stop()

	// a failed request leaves the protocol state unknown, so always retire
	// the process; the write errors are sticky and surface at the next wait
	fail := func(err error) (*AnalysisResponse, error) {
		drop(req.Model, eng)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	var sendErr error
	send := func(format string, args ...any) {
		if sendErr == nil {
			sendErr = eng.send(format, args...)
		}
	}
	waitFor := func(token string) error {
		if err := eng.waitFor(token); err != nil {
			if sendErr != nil {
				return sendErr
			}
			return err
		}
		return sendErr
	}

	// the process is reused, so reset every stateful option each request
	send("setoption name Elo value %d", elo)
	send("setoption name Temperature value %s", strconv.FormatFloat(temperature, 'f', -1, 64))
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
		return fail(err)
	}
	send("go nodes 1")

	lines := map[int]Line{}
	bestMove := ""
	for eng.scanner.Scan() {
		text := eng.scanner.Text()
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

	if bestMove == "" {
		if err := eng.scanner.Err(); err != nil {
			return fail(err)
		}
		return fail(fmt.Errorf("engine closed before bestmove"))
	}

	resp := &AnalysisResponse{BestMove: bestMove}
	for rank := 1; rank <= topN; rank++ {
		if line, ok := lines[rank]; ok {
			resp.Lines = append(resp.Lines, line)
		}
	}
	return resp, nil
}

// parseInfo pulls rank, score, wdl and pv out of one "info ..." search line.
// Lines without a pv, or with aspiration-window bound scores, are skipped;
// a missing score is allowed since maia may report policy-only lines.
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
		case "wdl":
			if i+3 < len(fields) {
				wdl := make([]int, 3)
				ok := true
				for j := range wdl {
					var err error
					if wdl[j], err = strconv.Atoi(fields[i+1+j]); err != nil {
						ok = false
						break
					}
				}
				if ok {
					line.WDL = wdl
				}
				i += 3
			}
		case "lowerbound", "upperbound":
			return Line{}, 0, false
		case "pv":
			line.PV = fields[i+1:]
			i = len(fields)
		}
	}

	if len(line.PV) == 0 {
		return Line{}, 0, false
	}
	line.Move = line.PV[0]
	return line, rank, true
}
