package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// useFakeCQL points the runner at testdata/fake-cql.sh.
func useFakeCQL(t *testing.T) {
	t.Helper()
	abs, err := filepath.Abs("testdata/fake-cql.sh")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CQL_PATH", abs)
}

func testRequest(query, pgn string, maxMatches int) QueryRequest {
	return QueryRequest{Query: query, PGN: pgn, MaxMatches: maxMatches}
}

const samplePGN = `[Event "F/S Return Match"]
[White "Fischer, Robert J."]
[Black "Spassky, Boris V."]

1. e4 e5 2. Nf3 1/2-1/2

[White "AlphaZero"]
[Black "Stockfish 8"]

1. d4 d5 2. c4
`

// TestParseOutputPGN covers the canned fake output: two games, three
// markers, with fens recovered by replay.
func TestParseOutputPGN(t *testing.T) {
	useFakeCQL(t)
	resp, err := runQuery(context.Background(), testRequest("cql().", samplePGN, 25), 25)
	if err != nil {
		t.Fatalf("runQuery: %v", err)
	}
	if resp.ScannedGames != 2 {
		t.Errorf("scanned_games = %d, want 2", resp.ScannedGames)
	}
	if resp.TotalMatches != 3 {
		t.Errorf("total_matches = %d, want 3", resp.TotalMatches)
	}
	if resp.Truncated {
		t.Error("truncated with no cap, want false")
	}

	// game 0: matches at start (ply 0), after 1. e4 e5 Nf3 (ply 3)
	m0 := resp.Matches[0]
	if m0.Game != 0 || m0.Ply != 0 {
		t.Errorf("matches[0] = game %d ply %d, want game 0 ply 0", m0.Game, m0.Ply)
	}
	if m0.Move != "" {
		t.Errorf("matches[0].move = %q, want empty (start position)", m0.Move)
	}
	if m0.White != "Fischer, Robert J." || m0.Black != "Spassky, Boris V." {
		t.Errorf("matches[0] headers = %q/%q", m0.White, m0.Black)
	}
	if m0.FEN == "" {
		t.Error("matches[0].fen missing")
	}
	// a ply-0 match must be the untouched start position
	if m0.FEN != "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1" {
		t.Errorf("matches[0].fen = %q, want start position", m0.FEN)
	}

	m1 := resp.Matches[1]
	if m1.Ply != 3 {
		t.Errorf("matches[1].ply = %d, want 3", m1.Ply)
	}
	if m1.Move != "Nf3" {
		t.Errorf("matches[1].move = %q, want Nf3", m1.Move)
	}
	if m1.FEN == "" {
		t.Error("matches[1].fen missing")
	}

	// game 1: match after 1. e4
	m2 := resp.Matches[2]
	if m2.Game != 1 || m2.Ply != 1 || m2.Move != "e4" {
		t.Errorf("matches[2] = game %d ply %d move %q, want game 1 ply 1 e4", m2.Game, m2.Ply, m2.Move)
	}
	if m2.White != "AlphaZero" {
		t.Errorf("matches[2].white = %q, want AlphaZero", m2.White)
	}
}

func TestParseOutputPGNTruncates(t *testing.T) {
	useFakeCQL(t)
	resp, err := runQuery(context.Background(), testRequest("cql().", samplePGN, 2), 2)
	if err != nil {
		t.Fatalf("runQuery: %v", err)
	}
	if len(resp.Matches) != 2 {
		t.Errorf("len(matches) = %d, want 2", len(resp.Matches))
	}
	if resp.TotalMatches != 3 {
		t.Errorf("total_matches = %d, want 3 (counted beyond the cap)", resp.TotalMatches)
	}
	if !resp.Truncated {
		t.Error("truncated, want true")
	}
}

func TestParseOutputPGNNoMatches(t *testing.T) {
	// no marker in the output — zero matches is a valid result
	dir := t.TempDir()
	out := filepath.Join(dir, "out.pgn")
	os.WriteFile(out, []byte(samplePGN), 0o600)

	resp, err := parseOutputPGN(out, 25)
	if err != nil {
		t.Fatalf("parseOutputPGN: %v", err)
	}
	if resp.TotalMatches != 0 || len(resp.Matches) != 0 || resp.Truncated {
		t.Errorf("response = %+v, want empty", resp)
	}
}

func TestCountGames(t *testing.T) {
	if got := countGames(samplePGN); got != 2 {
		t.Errorf("countGames = %d, want 2", got)
	}
	if got := countGames(""); got != 0 {
		t.Errorf("countGames(empty) = %d, want 0", got)
	}
}

func TestRunQueryMissingOutput(t *testing.T) {
	// fake writes nowhere: out.pgn never appears → error with stderr
	useFakeCQL(t)
	t.Setenv("FAKE_EXIT_CODE", "1")
	if _, err := runQuery(context.Background(), testRequest("cql().", samplePGN, 25), 25); err == nil {
		t.Error("runQuery with no output should fail")
	}
}

func TestRunQueryInputErrorCarriesDiagnostic(t *testing.T) {
	// cql rejected the input: exit status + stdout diagnostic → inputError
	// with the banner scrubbed out
	useFakeCQL(t)
	t.Setenv("FAKE_EXIT_CODE", "1")
	t.Setenv("FAKE_STDOUT", "CQL version 6.2 (build 9.493) (c) Gady\n\nCQL internal error: PgnSource: pgnAssert fails: missing end of game\n")

	_, err := runQuery(context.Background(), testRequest("cql().", samplePGN, 25), 25)
	if err == nil {
		t.Fatal("runQuery should fail")
	}
	var ie inputError
	if !errors.As(err, &ie) {
		t.Fatalf("error %q is not an inputError", err)
	}
	if !strings.Contains(ie.msg, "missing end of game") {
		t.Errorf("diagnostic = %q, want the actual complaint", ie.msg)
	}
	if strings.Contains(ie.msg, "CQL version") || strings.Contains(ie.msg, "CQL internal error") {
		t.Errorf("diagnostic = %q, want banner and framing scrubbed", ie.msg)
	}
}

func TestRunQueryTimeout(t *testing.T) {
	useFakeCQL(t)
	t.Setenv("FAKE_SLEEP", "1")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := runQuery(ctx, testRequest("cql().", samplePGN, 25), 25); err == nil ||
		!strings.Contains(err.Error(), "timed out") {
		t.Errorf("runQuery with a stalled cql = %v, want timeout error", err)
	}
}

func TestHandleQueryValidation(t *testing.T) {
	useFakeCQL(t)

	cases := []struct {
		name string
		body string
		want int
	}{
		{"missing query", `{"pgn":"1. e4"}`, http.StatusBadRequest},
		{"missing pgn", `{"query":"cql()."}`, http.StatusBadRequest},
		{"oversized query", `{"query":"` + strings.Repeat("x", maxQueryBytes+1) + `","pgn":"1. e4"}`, http.StatusBadRequest},
		{"bad json", `{`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/query", strings.NewReader(tc.body))
			rec := httptest.NewRecorder()
			handleQuery(rec, req)
			if rec.Code != tc.want {
				t.Errorf("got %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestHandleQueryOversizedPGN(t *testing.T) {
	useFakeCQL(t)
	big := strings.Repeat("x", maxPGNBytes+10)
	req := httptest.NewRequest("POST", "/query", strings.NewReader(`{"query":"cql().","pgn":"`+big+`"}`))
	rec := httptest.NewRecorder()
	handleQuery(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("oversized pgn got %d, want 400", rec.Code)
	}
}

func TestHandleQueryHappyPath(t *testing.T) {
	useFakeCQL(t)

	body := `{"query":"cql().","pgn":` + pgnJSON(samplePGN) + `,"max_matches":2}`
	req := httptest.NewRequest("POST", "/query", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handleQuery(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"scanned_games":2`) {
		t.Errorf("response missing scanned_games: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"truncated":true`) {
		t.Errorf("response missing truncation flag: %s", rec.Body.String())
	}
}

// pgnJSON encodes a pgn string as a json string value.
func pgnJSON(pgn string) string {
	b, err := json.Marshal(pgn)
	if err != nil {
		panic(err)
	}
	return string(b)
}
