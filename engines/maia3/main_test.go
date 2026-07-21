package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// useFakeEngine points the manager at testdata/fake-uci.sh and returns a
// function that reports how many engine processes have been spawned so far.
func useFakeEngine(t *testing.T) func() int {
	t.Helper()
	spawnLog := filepath.Join(t.TempDir(), "spawns")
	t.Setenv("ENGINE_CMD", "testdata/fake-uci.sh")
	t.Setenv("FAKE_SPAWN_LOG", spawnLog)
	t.Cleanup(resetRegistry)
	resetRegistry()
	return func() int {
		data, err := os.ReadFile(spawnLog)
		if err != nil {
			return 0
		}
		return len(strings.Fields(string(data)))
	}
}

func resetRegistry() {
	registry.Lock()
	defer registry.Unlock()
	for model, eng := range registry.engines {
		eng.kill()
		delete(registry.engines, model)
	}
}

func TestAnalyzeParsesEngineOutput(t *testing.T) {
	useFakeEngine(t)

	resp, err := analyze(context.Background(), AnalysisRequest{Model: "maia3-5m", TopN: 2})
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if resp.BestMove != "e2e4" {
		t.Errorf("bestmove = %q, want e2e4", resp.BestMove)
	}
	if len(resp.Lines) != 2 {
		t.Fatalf("len(lines) = %d, want 2", len(resp.Lines))
	}
	if !reflect.DeepEqual(resp.Lines[0].WDL, []int{310, 400, 290}) {
		t.Errorf("lines[0].wdl = %v, want [310 400 290]", resp.Lines[0].WDL)
	}
	if resp.Lines[1].WDL != nil {
		t.Errorf("lines[1].wdl = %v, want nil", resp.Lines[1].WDL)
	}
	if resp.Lines[0].Score.Value != 12 || resp.Lines[0].Score.Type != "cp" {
		t.Errorf("lines[0].score = %+v, want cp 12", resp.Lines[0].Score)
	}
	if !reflect.DeepEqual(resp.Lines[1].PV, []string{"d2d4", "d7d5"}) {
		t.Errorf("lines[1].pv = %v", resp.Lines[1].PV)
	}
}

func TestEngineProcessIsReused(t *testing.T) {
	spawns := useFakeEngine(t)

	for i := 0; i < 3; i++ {
		if _, err := analyze(context.Background(), AnalysisRequest{Model: "maia3-5m"}); err != nil {
			t.Fatalf("analyze %d: %v", i, err)
		}
	}
	if got := spawns(); got != 1 {
		t.Errorf("spawned %d processes for 3 requests, want 1", got)
	}
}

func TestDistinctModelsGetDistinctProcesses(t *testing.T) {
	spawns := useFakeEngine(t)

	for _, model := range []string{"maia3-5m", "maia3-79m"} {
		if _, err := analyze(context.Background(), AnalysisRequest{Model: model}); err != nil {
			t.Fatalf("analyze %s: %v", model, err)
		}
	}
	if got := spawns(); got != 2 {
		t.Errorf("spawned %d processes for 2 models, want 2", got)
	}
}

func TestCrashedEngineIsRespawned(t *testing.T) {
	spawns := useFakeEngine(t)

	if _, err := analyze(context.Background(), AnalysisRequest{Model: "maia3-5m"}); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	// kill the engine behind the manager's back, as a wedged process would
	registry.Lock()
	registry.engines["maia3-5m"].cmd.Process.Kill()
	registry.Unlock()
	// the dead process is only discovered on use: the next request fails
	// and retires it, and the one after gets a fresh engine
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := analyze(context.Background(), AnalysisRequest{Model: "maia3-5m"}); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("analyze kept succeeding on a killed engine")
		}
	}
	if _, err := analyze(context.Background(), AnalysisRequest{Model: "maia3-5m"}); err != nil {
		t.Fatalf("analyze after respawn: %v", err)
	}
	if got := spawns(); got != 2 {
		t.Errorf("spawned %d processes, want 2", got)
	}
}

func TestValidateGame(t *testing.T) {
	cases := []struct {
		name   string
		fen    string
		moves  []string
		wantOK bool
	}{
		{"startpos no moves", "", nil, true},
		{"startpos with moves", "", []string{"e2e4", "e7e5", "g1f3"}, true},
		{"custom fen with moves", "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1", []string{"d2d4"}, true},
		{"bad fen", "not a fen", nil, false},
		{"illegal move", "", []string{"e2e5"}, false},
		{"garbage move", "", []string{"e2e4\nquit"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateGame(tc.fen, tc.moves)
			if (err == nil) != tc.wantOK {
				t.Errorf("validateGame(%q, %v) = %v, want ok=%v", tc.fen, tc.moves, err, tc.wantOK)
			}
		})
	}
}
