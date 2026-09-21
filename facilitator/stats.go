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
