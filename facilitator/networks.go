package main

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	x402 "github.com/x402-foundation/x402/go/v2"
	batchsettlement "github.com/x402-foundation/x402/go/v2/mechanisms/evm/batch-settlement"
)

// chainConfig describes a built-in EVM network. The batch-settlement escrow
// contract is CREATE2-deployed at the same address on every chain, so adding
// a network is just an entry here — no other code changes.
type chainConfig struct {
	// Name is the human identifier used in the NETWORKS env var and in the
	// per-chain RPC override variable (RPC_URL_<NAME with - as _> uppercased).
	Name    string
	ChainID int64
	// DefaultRPCs are tried in order until one dials and reports the right
	// chain ID, so no RPC configuration is needed for built-in networks.
	DefaultRPCs []string
	Testnet     bool
}

func (c chainConfig) Network() x402.Network {
	return x402.Network(fmt.Sprintf("eip155:%d", c.ChainID))
}

// rpcEnvKey returns the per-chain override variable, mirroring the x402-rs
// facilitator convention: RPC_URL_BASE_SEPOLIA, RPC_URL_BSC, ...
func (c chainConfig) rpcEnvKey() string {
	return "RPC_URL_" + strings.ToUpper(strings.ReplaceAll(c.Name, "-", "_"))
}

var builtinChains = []chainConfig{
	{
		Name: "base-sepolia", ChainID: 84532, Testnet: true,
		DefaultRPCs: []string{"https://sepolia.base.org", "https://base-sepolia-rpc.publicnode.com"},
	},
	{
		Name: "base", ChainID: 8453,
		DefaultRPCs: []string{"https://mainnet.base.org", "https://base-rpc.publicnode.com"},
	},
	{
		Name: "bsc-testnet", ChainID: 97, Testnet: true,
		DefaultRPCs: []string{"https://data-seed-prebsc-1-s1.bnbchain.org:8545", "https://bsc-testnet-rpc.publicnode.com"},
	},
	{
		Name: "bsc", ChainID: 56,
		DefaultRPCs: []string{"https://bsc-dataseed.bnbchain.org", "https://bsc-rpc.publicnode.com"},
	},
}

// Polygon (137) and Arbitrum (42161) were built in during the multi-chain bring-up and
// removed on 2026-10-04: Unibase Pay settles on BNB Smart Chain and Base. Neither ever
// settled a payment. Re-adding one is an entry above and nothing else — the escrow is
// CREATE2-deployed at the same address on every chain.

// selectChains resolves the NETWORKS env var (comma-separated names, or "all"
// / "testnets" / "mainnets") against the built-in registry. Empty defaults to
// all built-in networks.
func selectChains(networksEnv string) ([]chainConfig, error) {
	spec := strings.TrimSpace(strings.ToLower(networksEnv))
	switch spec {
	case "", "all":
		return builtinChains, nil
	case "testnets", "mainnets":
		wantTestnet := spec == "testnets"
		out := make([]chainConfig, 0, len(builtinChains))
		for _, c := range builtinChains {
			if c.Testnet == wantTestnet {
				out = append(out, c)
			}
		}
		return out, nil
	}

	byName := make(map[string]chainConfig, len(builtinChains))
	for _, c := range builtinChains {
		byName[c.Name] = c
	}
	var out []chainConfig
	for _, raw := range strings.Split(spec, ",") {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		c, ok := byName[name]
		if !ok {
			known := make([]string, 0, len(builtinChains))
			for _, b := range builtinChains {
				known = append(known, b.Name)
			}
			return nil, fmt.Errorf("unknown network %q (built-in: %s)", name, strings.Join(known, ", "))
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("NETWORKS resolved to an empty set")
	}
	return out, nil
}

// addressFromPrivateKey derives the facilitator's address for startup logging
// without needing any RPC connection.
func addressFromPrivateKey(privateKeyHex string) (string, error) {
	pk, err := crypto.HexToECDSA(strings.TrimPrefix(privateKeyHex, "0x"))
	if err != nil {
		return "", err
	}
	return crypto.PubkeyToAddress(pk.PublicKey).Hex(), nil
}

// connectChain dials the chain's RPC (env override first, then the built-in
// defaults), verifies the chain ID matches, and checks the batch-settlement
// escrow contract actually has code there.
func connectChain(ctx context.Context, chain chainConfig, privateKey string) (*facilitatorEvmSigner, string, error) {
	candidates := chain.DefaultRPCs
	if override := os.Getenv(chain.rpcEnvKey()); override != "" {
		candidates = []string{override}
	}

	var lastErr error
	for _, rpcURL := range candidates {
		signer, err := newFacilitatorEvmSigner(privateKey, rpcURL)
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", rpcURL, err)
			continue
		}
		chainID, err := signer.GetChainID(ctx)
		if err != nil || chainID.Cmp(big.NewInt(chain.ChainID)) != 0 {
			if err == nil {
				err = fmt.Errorf("chain ID mismatch: got %s, want %d", chainID, chain.ChainID)
			}
			lastErr = fmt.Errorf("%s: %w", rpcURL, err)
			continue
		}
		code, err := signer.client.CodeAt(ctx, common.HexToAddress(batchsettlement.BatchSettlementAddress), nil)
		if err != nil {
			lastErr = fmt.Errorf("%s: read escrow code: %w", rpcURL, err)
			continue
		}
		if len(code) == 0 {
			return nil, "", fmt.Errorf("escrow contract %s not deployed on %s", batchsettlement.BatchSettlementAddress, chain.Name)
		}
		return signer, rpcURL, nil
	}
	return nil, "", lastErr
}
