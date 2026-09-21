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
// dashboard can show "BSC: 1,813 settlements" without re-adding the rows.
//
// TxCount is the network's own count (every successful tx the facilitator
// sent, including claims that move no tokens) — not a sum of per-asset counts,
// which would double-count multi-token txs. TotalAmount is the sum of every
// asset's amount in token units (base units / 10^decimals) — a stablecoin-
// shaped figure only when every asset on the network is USD-pegged. This
// process has no price feed, so it cannot do better than token units; the
// per-asset breakdown stays authoritative.
type networkSummary struct {
	TxCount     uint64 `json:"txCount"`
	TotalAmount string `json:"totalAmount"` // token units, decimal string
	Assets      int    `json:"assets"`
}

func summarize(networks map[string]*networkTotals) map[string]networkSummary {
	out := make(map[string]networkSummary, len(networks))
	for net, n := range networks {
		sum := new(big.Rat)
		maxDec := 0
		for _, t := range n.Assets {
			amt, ok := new(big.Int).SetString(t.TotalAmount, 10)
			if !ok {
				continue
			}
			den := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(t.Decimals)), nil)
			sum.Add(sum, new(big.Rat).SetFrac(amt, den))
			if t.Decimals > maxDec {
				maxDec = t.Decimals
			}
		}
		out[net] = networkSummary{TxCount: n.TxCount, TotalAmount: trimDecimal(sum.FloatString(maxDec)), Assets: len(n.Assets)}
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
