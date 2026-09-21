package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	x402 "github.com/x402-foundation/x402/go/v2"
)

// Onchain settlement indexer. Every settlement the facilitator performs is a
// transaction sent from its own address, so the chain is the complete history.
// Two feeds share one code path (indexTx) and dedupe by tx hash:
//   - live: the OnAfterSettle hook, as each settle lands
//   - backfill: an explorer `txlist` for the facilitator address, on startup
//     and every BACKFILL_INTERVAL, covering history and anything the live
//     hook missed (restarts, crashes between broadcast and hook).

// erc20TransferTopic = keccak256("Transfer(address,address,uint256)")
var erc20TransferTopic = common.HexToHash("0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef")

// indexTx reads a mined tx's receipt and records its token flow into stats.
// Amount per token = sum of ERC-20 Transfer values, minus pass-through hops
// (a Transfer whose `from` was a `to` earlier in the same tx — e.g. the
// batch-settlement deposit path payer → collector → escrow moves one amount
// through two Transfers). Txs with no Transfer (e.g. claims) still count as a tx.
func indexTx(ctx context.Context, stats *statsStore, metas *tokenMetaCache, signer *facilitatorEvmSigner, network x402.Network, hash string, block uint64) error {
	if stats.seen(hash) {
		return nil
	}
	receipt, err := signer.client.TransactionReceipt(ctx, common.HexToHash(hash))
	if err != nil {
		return fmt.Errorf("receipt %s: %w", hash, err)
	}
	if receipt.Status != 1 {
		return nil // reverted: not a settlement
	}
	if block == 0 && receipt.BlockNumber != nil {
		block = receipt.BlockNumber.Uint64()
	}

	amounts := map[string]*big.Int{}          // token -> net amount
	received := map[string]map[common.Address]bool{} // token -> addresses that were a `to`
	for _, lg := range receipt.Logs {
		if len(lg.Topics) != 3 || lg.Topics[0] != erc20TransferTopic || len(lg.Data) != 32 {
			continue
		}
		token := strings.ToLower(lg.Address.Hex())
		from := common.BytesToAddress(lg.Topics[1].Bytes())
		to := common.BytesToAddress(lg.Topics[2].Bytes())
		if received[token] == nil {
			received[token] = map[common.Address]bool{}
		}
		if !received[token][from] {
			if amounts[token] == nil {
				amounts[token] = new(big.Int)
			}
			amounts[token].Add(amounts[token], new(big.Int).SetBytes(lg.Data))
		}
		received[token][to] = true
	}

	assets := map[string]assetDelta{}
	for token, amt := range amounts {
		meta := metas.resolve(ctx, signer, string(network), token)
		assets[token] = assetDelta{symbol: meta.Symbol, decimals: meta.Decimals, amount: amt}
	}
	stats.recordTx(string(network), hash, block, assets)
	return nil
}

// --- explorer backfill ---

type explorerTx struct {
	Hash        string `json:"hash"`
	BlockNumber string `json:"blockNumber"`
	From        string `json:"from"`
	IsError     string `json:"isError"`
}

// explorerURL builds an Etherscan-compatible txlist URL. EXPLORER_URL_<NAME>
// overrides the base (e.g. a keyless Blockscout instance); otherwise Etherscan
// V2 is used and needs EXPLORER_API_KEY. Returns "" when neither is configured.
func explorerURL(chain chainConfig, address string, startBlock uint64, page int) string {
	base := os.Getenv("EXPLORER_URL_" + strings.ToUpper(strings.ReplaceAll(chain.Name, "-", "_")))
	key := os.Getenv("EXPLORER_API_KEY")
	if base == "" {
		if key == "" {
			return ""
		}
		base = fmt.Sprintf("https://api.etherscan.io/v2/api?chainid=%d", chain.ChainID)
	}
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	u := fmt.Sprintf("%s%smodule=account&action=txlist&address=%s&startblock=%d&endblock=99999999&page=%d&offset=1000&sort=asc",
		base, sep, address, startBlock, page)
	if key != "" && !strings.Contains(base, "blockscout") {
		u += "&apikey=" + key
	}
	return u
}

func fetchTxList(ctx context.Context, url string) ([]explorerTx, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var body struct {
		Status  string          `json:"status"`
		Message string          `json:"message"`
		Result  json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	var txs []explorerTx
	if err := json.Unmarshal(body.Result, &txs); err != nil {
		// Non-array result = "No transactions found" or an error string.
		if body.Status == "0" && strings.Contains(body.Message, "No transactions") {
			return nil, nil
		}
		return nil, fmt.Errorf("explorer: %s: %s", body.Message, strings.Trim(string(body.Result), `"`))
	}
	return txs, nil
}

// backfill indexes every successful tx sent by `address` on `chain` from the
// last indexed block onward. Public RPC/explorer friendly: sequential with
// small pauses. Safe to rerun — indexTx dedupes by hash.
func backfill(ctx context.Context, stats *statsStore, metas *tokenMetaCache, chain chainConfig, signer *facilitatorEvmSigner, address string) {
	network := string(chain.Network())
	start := stats.lastBlock(network)
	indexed := 0
	for page := 1; ; page++ {
		url := explorerURL(chain, address, start, page)
		if url == "" {
			return
		}
		txs, err := fetchTxList(ctx, url)
		if err != nil {
			fmt.Printf("[index] %s backfill: %v\n", chain.Name, err)
			break
		}
		for _, tx := range txs {
			if !strings.EqualFold(tx.From, address) || tx.IsError != "0" || stats.seen(tx.Hash) {
				continue
			}
			block, _ := strconv.ParseUint(tx.BlockNumber, 10, 64)
			if err := indexTx(ctx, stats, metas, signer, chain.Network(), tx.Hash, block); err != nil {
				fmt.Printf("[index] %s %v\n", chain.Name, err)
				continue
			}
			indexed++
			if indexed%100 == 0 {
				stats.save()
				fmt.Printf("[index] %s backfilled %d txs…\n", chain.Name, indexed)
			}
			time.Sleep(100 * time.Millisecond)
		}
		if len(txs) < 1000 {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if indexed > 0 {
		stats.save()
		fmt.Printf("[index] %s backfill done: %d new txs (through block %d)\n", chain.Name, indexed, stats.lastBlock(network))
	}
}

// runIndexer backfills all connected chains now and then every interval.
func runIndexer(stats *statsStore, metas *tokenMetaCache, chains []chainConfig, signers map[x402.Network]*facilitatorEvmSigner, address string, interval time.Duration) {
	run := func() {
		for _, chain := range chains {
			if signer := signers[chain.Network()]; signer != nil {
				backfill(context.Background(), stats, metas, chain, signer, address)
			}
		}
	}
	go func() {
		run()
		for range time.Tick(interval) {
			run()
		}
	}()
}
