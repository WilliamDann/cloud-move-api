

## Requirements
- users specify which engine to use for prediction
- users get a best move and eval from their engine
- maia predictions condition on the move history and imitate a target elo

## Data Model

Models (MODELS env, json array):
  id    : string // gateway model id (stockfish, maia3, maia3-xfast, ...)
  name  : string // display name
  url   : string // internal engine api url
  model : string // optional engine-side model name (maia3 checkpoint)
  elo   : int    // optional default elo when the request doesn't set one
  cache : bool   // memoize predict responses (lru + singleflight); only for
                 // deterministic analysis — never sampled engines like maia

## API Design

Auth: X-API-Key header, keys from API_KEYS env (comma separated).
Unset = open, for local dev. /health is always open.

### Engine Gateway
// gateway handles requests for prediction from engines

GET /models -> { models: {model_id, name}[] }

POST /predict
{
  model_id     : string
  position    ?: string   // base fen; standard start position if empty
  moves       ?: string[] // uci moves from position (lets maia condition on the line)
  depth       ?: int      // capped, stockfish only
  time        ?: int      // capped, seconds, stockfish only
  movetime_ms ?: int      // capped, overrides time, stockfish only
  top_n       ?: int      // capped, default 1
  elo         ?: int      // overrides the model's configured default, maia only
  temperature ?: float    // policy sampling, 0 = argmax, maia only
  skill       ?: int      // stockfish Skill Level 0-20, omit for full strength;
                          // skill-limited responses are never cached (randomized)
} -> { move, eval, pv, wdl?, lines: {move, score, pv, wdl?}[] }

POST /warm
// pre-load a model so the first predict skips the cold start; fire-and-forget
{ model_id : string } -> { model_id, already_warm }

GET /health -> { state: 'ok' }

### Engine API
// engine api handles talking with engines; one long-lived process per
// model stays warm across requests (loading weights is the expensive part)

POST /analysis
{
  position    ?: string
  moves       ?: string[]
  depth       ?: int      // stockfish
  time        ?: int      // stockfish, seconds
  top_n       ?: int
  elo         ?: int      // maia
  temperature ?: float    // maia
  model       ?: string   // maia checkpoint: maia3-5m | maia3-23m | maia3-79m
} -> { bestmove, lines: {move, score, pv, wdl?}[] }

POST /warm
{ model ?: string } -> { model, already_warm }  // no-op on stockfish

GET /health -> { state: 'ok' }
