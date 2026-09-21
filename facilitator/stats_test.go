package main

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func rat(s string) *big.Rat { r, _ := new(big.Rat).SetString(s); return r }

// Asset numbers are the 2026-09-21 production backfill. Without a price
// source there must be no USD field at all — not a token-unit sum dressed up
// as a total.
func TestSummarizeWithoutPrices(t *testing.T) {
	snap := map[string]*networkTotals{
		"bsc": {TxCount: 1814, Assets: map[string]*assetTotals{
			"0xf3a3": {Symbol: "XUSD", Decimals: 18, TxCount: 1796, TotalAmount: "13935168000000032400"},
			"0xce24": {Symbol: "U", Decimals: 18, TxCount: 15, TotalAmount: "14031000000000000000"},
			"0x0df0": {Symbol: "Cheems", Decimals: 18, TxCount: 1, TotalAmount: "10000000000000000000"},
			"0x8ac7": {Symbol: "USDC", Decimals: 18, TxCount: 1, TotalAmount: "100000000000000000"},
		}},
		"empty": {Assets: map[string]*assetTotals{}},
	}
	got := summarize(snap, nil)
	b := got["bsc"]
	if b.TxCount != 1814 || b.Assets != 4 || b.PricedAssets != 0 || b.UnpricedAssets != 4 {
		t.Fatalf("bsc = %+v", b)
	}
	if b.TotalUsd != "" || b.PriceSource != "" || b.PricedAt != "" {
		t.Fatalf("no price source must emit no USD fields: %+v", b)
	}
	if got["empty"].TxCount != 0 || got["empty"].Assets != 0 {
		t.Fatalf("empty = %+v", got["empty"])
	}
}

// Priced: only quoted assets sum into totalUsd (in USD, across decimals);
// the memecoin without a quote is counted in unpricedAssets, not at $1.
func TestSummarizeWithPrices(t *testing.T) {
	t1 := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	t2 := t1.Add(5 * time.Minute)
	snap := map[string]*networkTotals{"bsc": {TxCount: 3, Assets: map[string]*assetTotals{
		"0xusdc":   {Symbol: "USDC", Decimals: 6, TxCount: 1, TotalAmount: "1500000"},           // 1.5 USDC
		"0xu":      {Symbol: "U", Decimals: 18, TxCount: 1, TotalAmount: "2000000000000000000"}, // 2 U
		"0xcheems": {Symbol: "Cheems", Decimals: 18, TxCount: 1, TotalAmount: "10000000000000000000"},
	}}}
	quotes := map[string]usdQuote{
		"0xusdc": {Price: rat("0.9998"), At: t2},
		"0xu":    {Price: rat("1.25"), At: t1},
	}
	s := summarize(snap, quotes)["bsc"]
	// 1.5*0.9998 + 2*1.25 = 1.4997 + 2.5 = 3.9997
	if s.TotalUsd != "3.9997" || s.PricedAssets != 2 || s.UnpricedAssets != 1 || s.PriceSource != "coinmarketcap" {
		t.Fatalf("summary = %+v", s)
	}
	if s.PricedAt != t1.Format(time.RFC3339) {
		t.Fatalf("pricedAt should be the oldest quote used, got %s", s.PricedAt)
	}
	a := snap["bsc"].Assets
	if a["0xusdc"].ValueUsd != "1.4997" || a["0xusdc"].PriceUsd != "0.9998" || a["0xu"].ValueUsd != "2.5" {
		t.Fatalf("asset annotations: usdc=%+v u=%+v", a["0xusdc"], a["0xu"])
	}
	if a["0xcheems"].ValueUsd != "" || a["0xcheems"].PriceUsd != "" {
		t.Fatalf("unlisted asset must stay unpriced: %+v", a["0xcheems"])
	}
}

// Fake CMC: address lookup (listed → id, unlisted → 400), batched quotes.
// Second call must hit the cache — no new requests.
func TestCMCPricerQuotes(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if r.Header.Get("X-CMC_PRO_API_KEY") != "k" {
			t.Errorf("missing api key header")
		}
		switch r.URL.Path {
		case "/v2/cryptocurrency/info":
			if strings.EqualFold(r.URL.Query().Get("address"), "0xusdc") {
				w.Write([]byte(`{"status":{"error_code":0},"data":{"3408":{"id":3408,"symbol":"USDC"}}}`))
			} else {
				w.Write([]byte(`{"status":{"error_code":400,"error_message":"Invalid value for \"address\""}}`))
			}
		case "/v2/cryptocurrency/quotes/latest":
			if r.URL.Query().Get("id") != "3408" {
				t.Errorf("unexpected ids %q", r.URL.Query().Get("id"))
			}
			json.NewEncoder(w).Encode(map[string]any{"status": map[string]any{"error_code": 0}, "data": map[string]any{
				"3408": map[string]any{"quote": map[string]any{"USD": map[string]any{"price": 0.9998, "last_updated": "2026-09-21T10:00:00.000Z"}}},
			}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	p := newCMCPricer("k", time.Hour)
	p.baseURL = srv.URL
	q := p.Quotes(context.Background(), []string{"0xUSDC", "0xcheems"})
	if len(q) != 1 || q["0xusdc"].Price.FloatString(4) != "0.9998" {
		t.Fatalf("quotes = %+v", q)
	}
	if _, ok := q["0xcheems"]; ok {
		t.Fatalf("unlisted token must not be priced")
	}
	n := atomic.LoadInt32(&calls) // 2 info lookups + 1 quotes
	if n != 3 {
		t.Fatalf("expected 3 upstream calls, got %d", n)
	}
	p.Quotes(context.Background(), []string{"0xusdc", "0xcheems"})
	if atomic.LoadInt32(&calls) != n {
		t.Fatalf("second call should be fully cached (quote TTL + negative id cache)")
	}
}

func TestPegQuotesParseAndOverride(t *testing.T) {
	pegs := pegQuotes(" 0xF3a3E4D9C163251124229Da6DC9C98D889647804 , not-an-address, ,0x0df0587216a4a1bb7d5082fdc491d93d2dd4b413")
	if len(pegs) != 2 {
		t.Fatalf("want 2 pegs, got %d", len(pegs))
	}
	q, ok := pegs["0xf3a3e4d9c163251124229da6dc9c98d889647804"]
	if !ok || q.Price.Cmp(big.NewRat(1, 1)) != 0 || q.Source != "peg" || !q.At.IsZero() {
		t.Fatalf("peg not normalised: %+v", q)
	}
	if len(pegQuotes("")) != 0 {
		t.Fatal("empty spec must yield no pegs")
	}

	// The production shape: XUSD unlisted on CMC but pegged, U quoted by CMC.
	t1 := time.Date(2026, 9, 21, 4, 0, 0, 0, time.UTC)
	snap := map[string]*networkTotals{"bsc": {TxCount: 1811, Assets: map[string]*assetTotals{
		"0xf3a3e4d9c163251124229da6dc9c98d889647804": {Symbol: "XUSD", Decimals: 18, TxCount: 1796, TotalAmount: "13935168000000032400"},
		"0xce24439f2d9c6a2289f741120fe202248b666666": {Symbol: "U", Decimals: 18, TxCount: 15, TotalAmount: "14031000000000000000"},
	}}}
	quotes := map[string]usdQuote{"0xce24439f2d9c6a2289f741120fe202248b666666": {Price: rat("0.5"), At: t1, Source: "coinmarketcap"}}
	for a, q := range pegs {
		quotes[a] = q
	}
	s := summarize(snap, quotes)["bsc"]
	// 13.9351680000000324 * 1 + 14.031 * 0.5 = 20.9506680000000324 → 6 dp
	if s.TotalUsd != "20.950668" || s.PricedAssets != 2 || s.UnpricedAssets != 0 {
		t.Fatalf("summary = %+v", s)
	}
	if s.PriceSource != "coinmarketcap+peg" || s.PricedAt != t1.Format(time.RFC3339) {
		t.Fatalf("sources/pricedAt = %q %q", s.PriceSource, s.PricedAt)
	}
	x := snap["bsc"].Assets["0xf3a3e4d9c163251124229da6dc9c98d889647804"]
	if x.PriceUsd != "1" || x.ValueUsd != "13.935168" || x.PriceSource != "peg" {
		t.Fatalf("xusd = %+v", x)
	}

	// Pegs alone (no CMC key): priced, source "peg", no pricedAt to claim.
	only := summarize(map[string]*networkTotals{"bsc": {Assets: map[string]*assetTotals{
		"0xf3a3e4d9c163251124229da6dc9c98d889647804": {Symbol: "XUSD", Decimals: 18, TxCount: 1, TotalAmount: "1000000000000000000"},
	}}}, pegs)["bsc"]
	if only.TotalUsd != "1" || only.PriceSource != "peg" || only.PricedAt != "" {
		t.Fatalf("peg-only = %+v", only)
	}
}
