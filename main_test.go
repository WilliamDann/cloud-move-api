package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeEngine is an engine api stub that counts /analysis calls and can delay
// responses to force request overlap in concurrency tests.
type fakeEngine struct {
	calls atomic.Int64
	delay time.Duration
	fail  atomic.Bool // when set, /analysis responds 500 once
}

func (f *fakeEngine) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /analysis", func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		if f.fail.CompareAndSwap(true, false) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		time.Sleep(f.delay)
		json.NewEncoder(w).Encode(engineResponse{
			BestMove: "e2e4",
			Lines:    []Line{{Move: "e2e4", Score: Score{Type: "cp", Value: 20}, PV: []string{"e2e4"}}},
		})
	})
	return mux
}

// testRegistry wires a registry at one fake engine with cacheable and
// uncacheable models.
func testRegistry(engineURL string) *registry {
	reg := &registry{
		byID:    map[string]Model{},
		clients: map[string]*http.Client{},
		cache:   newResultCache(maxCacheEntries),
	}
	for _, m := range []Model{
		{ID: "sf", Name: "SF", URL: engineURL, Cache: true},
		{ID: "maia", Name: "Maia", URL: engineURL},
	} {
		reg.models = append(reg.models, m)
		reg.byID[m.ID] = m
	}
	reg.clients[engineURL] = &http.Client{}
	return reg
}

func predict(t *testing.T, reg *registry, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/predict", strings.NewReader(body))
	rec := httptest.NewRecorder()
	reg.handlePredict(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("predict returned %d: %s", rec.Code, rec.Body.String())
	}
	return rec
}

func TestCacheDedupesIdenticalRequests(t *testing.T) {
	engine := &fakeEngine{}
	srv := httptest.NewServer(engine.handler())
	defer srv.Close()
	reg := testRegistry(srv.URL)

	body := `{"model_id":"sf","moves":["e2e4"],"depth":10}`
	first := predict(t, reg, body)
	second := predict(t, reg, body)

	if got := engine.calls.Load(); got != 1 {
		t.Errorf("engine called %d times for identical requests, want 1", got)
	}
	if h := first.Header().Get("X-Cache"); h != "MISS" {
		t.Errorf("first X-Cache = %q, want MISS", h)
	}
	if h := second.Header().Get("X-Cache"); h != "HIT" {
		t.Errorf("second X-Cache = %q, want HIT", h)
	}
}

func TestCacheKeyIncludesRequestFields(t *testing.T) {
	engine := &fakeEngine{}
	srv := httptest.NewServer(engine.handler())
	defer srv.Close()
	reg := testRegistry(srv.URL)

	predict(t, reg, `{"model_id":"sf","moves":["e2e4"],"depth":10}`)
	predict(t, reg, `{"model_id":"sf","moves":["e2e4"],"depth":12}`)
	predict(t, reg, `{"model_id":"sf","moves":["d2d4"],"depth":10}`)

	if got := engine.calls.Load(); got != 3 {
		t.Errorf("engine called %d times for 3 distinct requests, want 3", got)
	}
}

func TestConcurrentIdenticalRequestsCoalesce(t *testing.T) {
	engine := &fakeEngine{delay: 100 * time.Millisecond}
	srv := httptest.NewServer(engine.handler())
	defer srv.Close()
	reg := testRegistry(srv.URL)

	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest("POST", "/predict", strings.NewReader(`{"model_id":"sf","moves":["e2e4"],"depth":10}`))
			rec := httptest.NewRecorder()
			reg.handlePredict(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("predict returned %d", rec.Code)
			}
		}()
	}
	wg.Wait()

	if got := engine.calls.Load(); got != 1 {
		t.Errorf("engine called %d times for %d concurrent identical requests, want 1", got, n)
	}
}

func TestSkillRequestsBypassCache(t *testing.T) {
	engine := &fakeEngine{}
	srv := httptest.NewServer(engine.handler())
	defer srv.Close()
	reg := testRegistry(srv.URL)

	// skill-limited moves are randomized — identical requests must each
	// reach the engine even on a cache-enabled model
	body := `{"model_id":"sf","moves":["e2e4"],"skill":5,"movetime_ms":500}`
	rec := predict(t, reg, body)
	predict(t, reg, body)

	if got := engine.calls.Load(); got != 2 {
		t.Errorf("engine called %d times for skill-limited requests, want 2", got)
	}
	if h := rec.Header().Get("X-Cache"); h != "" {
		t.Errorf("skill-limited request set X-Cache = %q, want unset", h)
	}
}

func TestUncachedModelBypassesCache(t *testing.T) {
	engine := &fakeEngine{}
	srv := httptest.NewServer(engine.handler())
	defer srv.Close()
	reg := testRegistry(srv.URL)

	body := `{"model_id":"maia","moves":["e2e4"]}`
	rec := predict(t, reg, body)
	predict(t, reg, body)

	if got := engine.calls.Load(); got != 2 {
		t.Errorf("engine called %d times for uncached model, want 2", got)
	}
	if h := rec.Header().Get("X-Cache"); h != "" {
		t.Errorf("uncached model set X-Cache = %q, want unset", h)
	}
}

func TestErrorsAreNotCached(t *testing.T) {
	engine := &fakeEngine{}
	engine.fail.Store(true)
	srv := httptest.NewServer(engine.handler())
	defer srv.Close()
	reg := testRegistry(srv.URL)

	body := `{"model_id":"sf","moves":["e2e4"],"depth":10}`
	req := httptest.NewRequest("POST", "/predict", strings.NewReader(body))
	rec := httptest.NewRecorder()
	reg.handlePredict(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("failing engine returned %d, want 502", rec.Code)
	}

	predict(t, reg, body) // engine healthy again — must retry, not serve the error
	if got := engine.calls.Load(); got != 2 {
		t.Errorf("engine called %d times, want 2 (error must not be cached)", got)
	}
}

func TestLRUEviction(t *testing.T) {
	c := newResultCache(2)
	fetchCount := 0
	fetch := func(move string) func() (engineResponse, error) {
		return func() (engineResponse, error) {
			fetchCount++
			return engineResponse{BestMove: move}, nil
		}
	}

	c.do("a", fetch("a1"))
	c.do("b", fetch("b1"))
	c.do("a", fetch("a2")) // refresh a's recency — b is now least recent
	c.do("c", fetch("c1")) // evicts b

	if _, hit, _ := c.do("a", fetch("a3")); !hit {
		t.Error("a should still be cached (recency was refreshed)")
	}
	if resp, hit, _ := c.do("b", fetch("b2")); hit {
		t.Error("b should have been evicted")
	} else if resp.BestMove != "b2" {
		t.Errorf("refetched b = %q, want b2", resp.BestMove)
	}
	if fetchCount != 4 {
		t.Errorf("fetch ran %d times, want 4 (a, b, c, refetched b)", fetchCount)
	}
}

func TestPredictKeyIsolatesModels(t *testing.T) {
	// same request body under two model ids must not share cache entries
	c := newResultCache(8)
	c.do("sf|body", func() (engineResponse, error) { return engineResponse{BestMove: "x"}, nil })
	if _, hit, _ := c.do("other|body", func() (engineResponse, error) { return engineResponse{BestMove: "y"}, nil }); hit {
		t.Error("different model ids collided in the cache")
	}
}

func TestValidateGameGateway(t *testing.T) {
	if err := validateGame("", []string{"e2e4", "e7e5"}); err != nil {
		t.Errorf("legal moves rejected: %v", err)
	}
	if err := validateGame("", []string{"e2e5"}); err == nil {
		t.Error("illegal move accepted")
	}
	if err := validateGame("not a fen", nil); err == nil {
		t.Error("bad fen accepted")
	}
	if err := validateGame("", make([]string, maxMoves+1)); err == nil {
		t.Error("oversized move list accepted")
	}
}

// fakeCQLEngine is a cql engine stub that echoes a canned response and
// records the last request body for assertion.
func fakeCQLEngine(t *testing.T, status int, response string) (*httptest.Server, *atomic.Value) {
	t.Helper()
	var lastBody atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		lastBody.Store(string(body))
		w.WriteHeader(status)
		w.Write([]byte(response))
	}))
	t.Cleanup(srv.Close)
	return srv, &lastBody
}

func queryRequest(t *testing.T, reg *registry, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/query", strings.NewReader(body))
	req.Body = http.MaxBytesReader(httptest.NewRecorder(), req.Body, maxQueryBytes+maxPGNBytes+(1<<20))
	rec := httptest.NewRecorder()
	reg.handleQuery(rec, req)
	return rec
}

func TestQueryNotConfigured(t *testing.T) {
	reg := testRegistry("http://unused")
	reg.cqlURL = ""

	rec := queryRequest(t, reg, `{"query":"cql().","pgn":"1. e4"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("unconfigured engine got %d, want 503", rec.Code)
	}
}

func TestQueryValidation(t *testing.T) {
	srv, _ := fakeCQLEngine(t, 200, `{}`)
	reg := testRegistry(srv.URL)
	reg.cqlURL = srv.URL
	reg.clients[srv.URL] = srv.Client()

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
			rec := queryRequest(t, reg, tc.body)
			if rec.Code != tc.want {
				t.Errorf("got %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestQueryProxiesResponse(t *testing.T) {
	const engineResponse = `{"matches":[{"game":0,"ply":4}],"total_matches":1,"truncated":false,"scanned_games":2}`
	srv, lastBody := fakeCQLEngine(t, 200, engineResponse)
	reg := testRegistry(srv.URL)
	reg.cqlURL = srv.URL
	reg.clients[srv.URL] = srv.Client()

	rec := queryRequest(t, reg, `{"query":"cql().","pgn":"1. e4","max_matches":900}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != engineResponse {
		t.Errorf("body = %q, want engine response passed through", rec.Body.String())
	}

	// the upstream request must carry the clamped max_matches
	upstream := lastBody.Load().(string)
	if !strings.Contains(upstream, `"max_matches":100`) {
		t.Errorf("upstream body = %s, want clamped max_matches 100", upstream)
	}
}

func TestQueryEngineErrorPassesThrough(t *testing.T) {
	srv, _ := fakeCQLEngine(t, http.StatusGatewayTimeout, "query timed out")
	reg := testRegistry(srv.URL)
	reg.cqlURL = srv.URL
	reg.clients[srv.URL] = srv.Client()

	rec := queryRequest(t, reg, `{"query":"cql().","pgn":"1. e4"}`)
	if rec.Code != http.StatusGatewayTimeout {
		t.Errorf("got %d, want 504 passed through", rec.Code)
	}
}

func TestQueryEngineDown(t *testing.T) {
	reg := testRegistry("http://127.0.0.1:1") // nothing listens here
	reg.cqlURL = "http://127.0.0.1:1"
	reg.clients["http://127.0.0.1:1"] = &http.Client{Timeout: time.Second}

	rec := queryRequest(t, reg, `{"query":"cql().","pgn":"1. e4"}`)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("got %d, want 502", rec.Code)
	}
}

func BenchmarkCacheHit(b *testing.B) {
	c := newResultCache(1024)
	c.do("k", func() (engineResponse, error) { return engineResponse{BestMove: "e2e4"}, nil })
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		c.get("k")
	}
}
