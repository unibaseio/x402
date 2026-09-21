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

// Settlement stats: per network × asset, count of onchain txs and total
// settled amount in base units. Fed by the OnAfterSettle hook, persisted as
// one JSON file, served at GET /stats.
// ponytail: single JSON file + global mutex; move to SQLite if write volume
// or multi-instance deployment ever demands it.

type assetTotals struct {
	Symbol      string `json:"symbol"`
	Decimals    int    `json:"decimals"`
	TxCount     uint64 `json:"txCount"`
	TotalAmount string `json:"totalAmount"` // base units (big.Int decimal string)
}

type statsStore struct {
	mu       sync.Mutex
	path     string
	Networks map[string]map[string]*assetTotals `json:"networks"` // network -> asset address -> totals
}

func loadStats(path string) *statsStore {
	s := &statsStore{path: path, Networks: map[string]map[string]*assetTotals{}}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, s); err != nil {
			fmt.Printf("[stats] %s corrupt (%v), starting fresh\n", path, err)
		}
	}
	return s
}

// record adds one successful onchain settlement. Zero-amount records still
// bump the tx count.
func (s *statsStore) record(network, asset, symbol string, decimals int, amount *big.Int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	byAsset := s.Networks[network]
	if byAsset == nil {
		byAsset = map[string]*assetTotals{}
		s.Networks[network] = byAsset
	}
	key := strings.ToLower(asset)
	t := byAsset[key]
	if t == nil {
		t = &assetTotals{Symbol: symbol, Decimals: decimals, TotalAmount: "0"}
		byAsset[key] = t
	}
	t.TxCount++
	total, _ := new(big.Int).SetString(t.TotalAmount, 10)
	if total == nil {
		total = big.NewInt(0)
	}
	t.TotalAmount = total.Add(total, amount).String()

	// Persist inline: settles are onchain-tx-rate, i.e. rare.
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err == nil {
		if data, err := json.MarshalIndent(s, "", "  "); err == nil {
			if err := os.WriteFile(s.path, data, 0o644); err != nil {
				fmt.Printf("[stats] write %s: %v\n", s.path, err)
			}
		}
	} else {
		fmt.Printf("[stats] mkdir for %s: %v\n", s.path, err)
	}
}

func (s *statsStore) snapshot() map[string]map[string]*assetTotals {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]map[string]*assetTotals, len(s.Networks))
	for n, byAsset := range s.Networks {
		cp := make(map[string]*assetTotals, len(byAsset))
		for a, t := range byAsset {
			c := *t
			cp[a] = &c
		}
		out[n] = cp
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
