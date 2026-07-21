# cloud-move

Chess engine gateway. Exposes a single HTTP API over multiple engine services
([Stockfish](https://github.com/official-stockfish/Stockfish),
[Maia 3](https://github.com/CSSLab/maia3)), validates requests, and proxies
them to the right engine.
Runs on Cloud Run; engines are separate services behind it (see `engines/`).

## API

Auth: `X-API-Key` header (keys from `API_KEYS`, comma separated; unset = open
for local dev). `/health` is always open.

- `GET /health` → `{ state: "ok" }`
- `GET /models` → `{ models: [{ model_id, name }] }`
- `POST /predict` → best move + eval for a position
  ```json
  {
    "model_id": "stockfish",
    "position": "<fen, optional>",
    "moves": ["e2e4", "e7e5"],
    "depth": 12,
    "top_n": 3
  }
  ```
  Returns `{ move, eval, pv, wdl?, lines }`. Maia-only knobs: `elo`,
  `temperature`; Stockfish-only: `depth`, `time`, `movetime_ms`, `skill`.
- `POST /warm` `{ model_id }` — pre-load a model so the first predict skips
  the cold start; fire-and-forget.

Full request/response schemas in [design](design).

## Configuration

| Env        | Purpose                                             |
|------------|-----------------------------------------------------|
| `MODELS`   | JSON array of models (id, name, url, model, elo, cache) |
| `API_KEYS` | Comma-separated API keys; unset disables auth       |
| `PORT`     | Listen port (default 8080)                          |

Deterministic models with `cache: true` get an LRU + singleflight response
cache (skill-limited requests are never cached).

## Run locally

```sh
docker compose up --build   # gateway :8080, stockfish :8081, maia3 :8082
go test ./...
```
