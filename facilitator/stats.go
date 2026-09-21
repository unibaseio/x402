package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Settlement stats: per network, onchain tx count and per-asset total amount
// (base units), indexed from tx receipts (see indexer.go). One JSON file,
// served at GET /stats.
// ponytail: single JSON file + global mutex + in-file seen-set; move to
// SQLite if tx volume or multi-instance deployment ever demands it.

type assetTotals struct {
	Symbol      string `json:"symbol"`
	Decimals    int    `json:"decimals"`
	TxCount     uint64 `json:"txCount"`
	TotalAmount string `json:"totalAmount"` // base units (big.Int decimal string)
	// Set per request by summarize when a price source is configured and CMC
	// lists the token; absent otherwise (never fabricated).
	PriceUsd    string `json:"priceUsd,omitempty"`
	ValueUsd    string `json:"valueUsd,omitempty"`
	PriceSource string `json:"priceSource,omitempty"` // "coinmarketcap" | "peg"
}

type networkTotals struct {
	TxCount   uint64                  `json:"txCount"`   // every successful tx the facilitator sent
	LastBlock uint64                  `json:"lastBlock"` // highest block indexed (backfill resumes here)
	Assets    map[string]*assetTotals `json:"assets"`    // token address -> totals
}

type statsStore struct {
	mu       sync.Mutex
	path     string
	Networks map[string]*networkTotals `json:"networks"`
	Seen     map[string]bool           `json:"seen"` // tx hash -> indexed (dedupes live hook vs backfill)
}

type assetDelta struct {
	symbol   string
	decimals int
	amount   *big.Int
}

func loadStats(path string) *statsStore {
	s := &statsStore{path: path}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, s); err != nil {
			fmt.Printf("[stats] %s corrupt (%v), starting fresh\n", path, err)
		}
	}
	if s.Networks == nil {
		s.Networks = map[string]*networkTotals{}
	}
	if s.Seen == nil {
		s.Seen = map[string]bool{}
	}
	return s
}

func (s *statsStore) seen(hash string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Seen[strings.ToLower(hash)]
}

func (s *statsStore) lastBlock(network string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n := s.Networks[network]; n != nil {
		return n.LastBlock
	}
	return 0
}

// recordTx adds one indexed tx. Idempotent per hash. Does not save — callers
// batch saves (backfill) or save right after (live hook).
func (s *statsStore) recordTx(network, hash string, block uint64, assets map[string]assetDelta) {
	s.mu.Lock()
	defer s.mu.Unlock()

	hash = strings.ToLower(hash)
	if s.Seen[hash] {
		return
	}
	s.Seen[hash] = true

	n := s.Networks[network]
	if n == nil {
		n = &networkTotals{Assets: map[string]*assetTotals{}}
		s.Networks[network] = n
	}
	n.TxCount++
	if block > n.LastBlock {
		n.LastBlock = block
	}
	for token, d := range assets {
		key := strings.ToLower(token)
		t := n.Assets[key]
		if t == nil {
			t = &assetTotals{Symbol: d.symbol, Decimals: d.decimals, TotalAmount: "0"}
			n.Assets[key] = t
		}
		t.TxCount++
		total, ok := new(big.Int).SetString(t.TotalAmount, 10)
		if !ok {
			total = new(big.Int)
		}
		t.TotalAmount = total.Add(total, d.amount).String()
	}
}

func (s *statsStore) save() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		fmt.Printf("[stats] mkdir for %s: %v\n", s.path, err)
		return
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(s.path, data, 0o644); err != nil {
		fmt.Printf("[stats] write %s: %v\n", s.path, err)
	}
}

// snapshot returns a deep copy of the network totals (without the seen-set).
func (s *statsStore) snapshot() map[string]*networkTotals {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]*networkTotals, len(s.Networks))
	for name, n := range s.Networks {
		cp := &networkTotals{TxCount: n.TxCount, LastBlock: n.LastBlock, Assets: make(map[string]*assetTotals, len(n.Assets))}
		for a, t := range n.Assets {
			c := *t
			cp.Assets[a] = &c
		}
		out[name] = cp
	}
	return out
}

// Per-network rollup of the snapshot, served next to the per-asset detail so a
// dashboard can show "BSC: 1,814 settlements, $X" without re-adding rows.
//
// TxCount is the network's own count (every successful tx the facilitator
// sent, including claims that move no tokens) — not a sum of per-asset counts,
// which would double-count multi-token txs.
//
// TotalUsd only ever sums assets that have a USD quote; a token the price
// source does not list is reported in UnpricedAssets instead of being counted
// at face value (a memecoin and a stablecoin are not 1:1). Without a price
// source, no USD field is emitted at all.
type networkSummary struct {
	TxCount        uint64 `json:"txCount"`
	Assets         int    `json:"assets"`
	PricedAssets   int    `json:"pricedAssets"`
	UnpricedAssets int    `json:"unpricedAssets"`
	TotalUsd       string `json:"totalUsd,omitempty"`    // sum over priced assets, USD
	PriceSource    string `json:"priceSource,omitempty"` // sources used by priced assets: "coinmarketcap", "peg", or "coinmarketcap+peg"
	PricedAt       string `json:"pricedAt,omitempty"`    // oldest market quote used, RFC3339 UTC; absent when only pegs priced
}

// summarize builds the per-network rollup and, when quotes are given,
// annotates each asset in `networks` with priceUsd / valueUsd. quotes == nil
// means no price source is configured.
func summarize(networks map[string]*networkTotals, quotes map[string]usdQuote) map[string]networkSummary {
	out := make(map[string]networkSummary, len(networks))
	for net, n := range networks {
		sum := new(big.Rat)
		var oldest time.Time
		priced := 0
		usedCMC, usedPeg := false, false
		for addr, t := range n.Assets {
			q, ok := quotes[strings.ToLower(addr)]
			if !ok || q.Price == nil {
				continue
			}
			amt, ok := new(big.Int).SetString(t.TotalAmount, 10)
			if !ok {
				continue
			}
			den := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(t.Decimals)), nil)
			value := new(big.Rat).Mul(new(big.Rat).SetFrac(amt, den), q.Price)
			t.PriceUsd = trimDecimal(q.Price.FloatString(8))
			t.ValueUsd = trimDecimal(value.FloatString(6))
			t.PriceSource = q.Source
			sum.Add(sum, value)
			priced++
			switch q.Source {
			case "peg":
				usedPeg = true
			default:
				usedCMC = true
			}
			// Pegs carry no timestamp; pricedAt reflects market data only.
			if !q.At.IsZero() && (oldest.IsZero() || q.At.Before(oldest)) {
				oldest = q.At
			}
		}
		ns := networkSummary{TxCount: n.TxCount, Assets: len(n.Assets), PricedAssets: priced, UnpricedAssets: len(n.Assets) - priced}
		switch {
		case usedCMC && usedPeg:
			ns.PriceSource = "coinmarketcap+peg"
		case usedCMC:
			ns.PriceSource = "coinmarketcap"
		case usedPeg:
			ns.PriceSource = "peg"
		}
		if priced > 0 {
			ns.TotalUsd = trimDecimal(sum.FloatString(6))
		}
		if !oldest.IsZero() {
			ns.PricedAt = oldest.UTC().Format(time.RFC3339)
		}
		out[net] = ns
	}
	return out
}

// trimDecimal drops trailing zeros (and a bare trailing point): "38.066168000" → "38.066168", "14.0" → "14".
func trimDecimal(s string) string {
	if !strings.Contains(s, ".") {
		return s
	}
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// --- token metadata (symbol/decimals) with cache ---

var erc20SymbolABI = []byte(`[{"name":"symbol","type":"function","stateMutability":"view","inputs":[],"outputs":[{"type":"string"}]}]`)
var erc20DecimalsABI = []byte(`[{"name":"decimals","type":"function","stateMutability":"view","inputs":[],"outputs":[{"type":"uint8"}]}]`)

type tokenMeta struct {
	Symbol   string
	Decimals int
}

type tokenMetaCache struct {
	mu    sync.Mutex
	cache map[string]tokenMeta // network|asset -> meta
}

func newTokenMetaCache() *tokenMetaCache {
	return &tokenMetaCache{cache: map[string]tokenMeta{}}
}

// resolve fetches symbol()/decimals() once per (network, asset); on RPC
// failure it falls back to the raw address so stats still record.
func (c *tokenMetaCache) resolve(ctx context.Context, signer *facilitatorEvmSigner, network, asset string) tokenMeta {
	key := network + "|" + strings.ToLower(asset)
	c.mu.Lock()
	if m, ok := c.cache[key]; ok {
		c.mu.Unlock()
		return m
	}
	c.mu.Unlock()

	meta := tokenMeta{Symbol: asset, Decimals: 0}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if signer != nil {
		if v, err := signer.ReadContract(rctx, asset, erc20SymbolABI, "symbol"); err == nil {
			if sym, ok := v.(string); ok && sym != "" {
				meta.Symbol = sym
			}
		}
		if v, err := signer.ReadContract(rctx, asset, erc20DecimalsABI, "decimals"); err == nil {
			if d, ok := v.(uint8); ok {
				meta.Decimals = int(d)
			}
		}
	}
	// Only cache successful lookups so a transient RPC failure doesn't pin the
	// bare-address fallback forever.
	if meta.Symbol != asset {
		c.mu.Lock()
		c.cache[key] = meta
		c.mu.Unlock()
	}
	return meta
}
