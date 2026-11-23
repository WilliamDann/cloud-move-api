package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/notnil/chess"
)

// token is one mainline movetext item: a san move or a comment.
type token struct {
	text    string
	comment bool // text is a {} comment body
}

var (
	resultRe = regexp.MustCompile(`^(1-0|0-1|1/2-1/2|\*)$`)
	nagRe    = regexp.MustCompile(`^\$\d+$`)
	moveNum  = regexp.MustCompile(`^\d+\.+`)
	sanRe    = regexp.MustCompile(`^(O-O-O|O-O|0-0-0|0-0|(O-O-O|O-O|0-0-0|0-0|[KQRBNa-h][a-h1-8]?x?[a-h]?[1-8]?)(=[KQRBN])?)[+#]?[!?]*$`)
)

// sanFromToken strips move numbers, NAGs and results and reports whether
// what remains is a san move.
func sanFromToken(tok string) (string, bool) {
	if resultRe.MatchString(tok) || nagRe.MatchString(tok) {
		return "", false
	}
	tok = moveNum.ReplaceAllString(tok, "")
	if tok == "" {
		return "", false
	}
	return tok, sanRe.MatchString(tok)
}

// tokenize scans movetext in order, yielding mainline san moves and
// comments. Variations (…), line comments (;) and variation-internal
// content are skipped so ply numbers count the mainline only.
func tokenize(movetext string) []token {
	var toks []token
	depth := 0 // inside ( ... )
	for i := 0; i < len(movetext); {
		c := movetext[i]
		switch {
		case c == '{':
			j := strings.IndexByte(movetext[i+1:], '}')
			end := len(movetext)
			if j >= 0 {
				end = i + 1 + j
			}
			body := movetext[i+1 : end]
			if depth == 0 {
				toks = append(toks, token{text: body, comment: true})
			}
			i = end + 1
		case c == ';':
			j := strings.IndexByte(movetext[i:], '\n')
			if j < 0 {
				i = len(movetext)
			} else {
				i += j + 1
			}
		case c == '(':
			depth++
			i++
		case c == ')':
			if depth > 0 {
				depth--
			}
			i++
		case c == ' ' || c == '\n' || c == '\r' || c == '\t':
			i++
		case depth == 0:
			j := i
			for j < len(movetext) {
				c := movetext[j]
				if c == ' ' || c == '\n' || c == '\r' || c == '\t' ||
					c == '{' || c == ';' || c == '(' || c == ')' {
					break
				}
				j++
			}
			toks = append(toks, token{text: movetext[i:j]})
			i = j
		default:
			i++
		}
	}
	return toks
}

var tagRe = regexp.MustCompile(`^\[\s*(\w+)\s+"(.*)"\s*\]\s*$`)

// pgnGame is one game: its tags and raw movetext.
type pgnGame struct {
	headers  map[string]string
	movetext string
}

// splitGames splits a pgn file into games. A tag block that follows
// movetext starts the next game. It is a structural scan, not a validator
// — cql does the authoritative parsing on the same bytes.
func splitGames(pgn string) []pgnGame {
	var games []pgnGame
	var headers map[string]string
	var movetext strings.Builder
	sawMovetext := false

	flush := func() {
		if headers != nil || movetext.Len() > 0 {
			games = append(games, pgnGame{headers: headers, movetext: movetext.String()})
		}
		headers = nil
		movetext.Reset()
	}

	for _, line := range strings.Split(pgn, "\n") {
		if m := tagRe.FindStringSubmatch(line); m != nil {
			if sawMovetext { // next game begins
				flush()
				sawMovetext = false
			}
			if headers == nil {
				headers = map[string]string{}
			}
			headers[m[1]] = m[2]
			continue
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		// anything else is movetext
		sawMovetext = true
		movetext.WriteString(line)
		movetext.WriteString("\n")
	}
	flush()
	return games
}

func countGames(pgn string) int {
	return len(splitGames(pgn))
}

// parseOutputPGN reads a cql output file and extracts matches. topN
// bounds the returned match list; all matches are counted. Matched games
// are replayed (up to their last matched ply) to recover fens.
func parseOutputPGN(path string, topN int) (*QueryResponse, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading output: %w", err)
	}

	resp := &QueryResponse{}
	for gi, game := range splitGames(string(raw)) {
		toks := tokenize(game.movetext)

		// match plies in this game, in movetext order
		var plies []int
		sanCount := 0
		for _, tok := range toks {
			if tok.comment {
				if strings.Contains(tok.text, matchMarker) {
					plies = append(plies, sanCount)
				}
				continue
			}
			if _, ok := sanFromToken(tok.text); ok {
				sanCount++
			}
		}
		resp.TotalMatches += len(plies)
		if len(plies) == 0 || len(resp.Matches) >= topN {
			continue
		}

		fens, moves := replay(game, plies)
		for _, ply := range plies {
			if len(resp.Matches) >= topN {
				break
			}
			m := Match{Game: gi, Ply: ply, Move: moves[ply]}
			if fen, ok := fens[ply]; ok {
				m.FEN = fen
			}
			m.White = game.headers["White"]
			m.Black = game.headers["Black"]
			m.Date = game.headers["Date"]
			resp.Matches = append(resp.Matches, m)
		}
	}
	resp.Truncated = resp.TotalMatches > len(resp.Matches)
	return resp, nil
}

// replay applies a game's mainline moves with notnil/chess, returning the
// fen and the san leading into each matched ply. Plies that fail to
// decode (cql's san dialect vs ours, or a broken game) simply get no fen.
func replay(game pgnGame, plies []int) (map[int]string, map[int]string) {
	fens := map[int]string{}
	moves := map[int]string{}
	lastPly := plies[len(plies)-1]

	g := chess.NewGame()
	if fen, ok := game.headers["FEN"]; ok && fen != "" {
		base, err := chess.FEN(fen)
		if err == nil {
			g = chess.NewGame(base)
		}
	}
	startFEN := g.FEN() // captured before any move mutates the game

	san := chess.AlgebraicNotation{}
	seen := 0
	for _, tok := range tokenize(game.movetext) {
		if tok.comment {
			continue
		}
		text, ok := sanFromToken(tok.text)
		if !ok {
			continue
		}
		if seen == lastPly {
			break
		}
		move, err := san.Decode(g.Position(), text)
		if err != nil {
			break // stop trusting the replay; remaining fens stay unset
		}
		if err := g.Move(move); err != nil {
			break
		}
		seen++
		if _, want := lookup(plies, seen); want {
			fens[seen] = g.FEN()
			moves[seen] = text
		}
	}
	// a match at ply 0 (before any move) gets the start position fen
	if _, want := lookup(plies, 0); want {
		fens[0] = startFEN
	}
	return fens, moves
}

func lookup(plies []int, ply int) (int, bool) {
	for _, p := range plies {
		if p == ply {
			return p, true
		}
	}
	return 0, false
}
