// Command facilitator is a standalone x402 v2 facilitator that speaks the
// batch-settlement EVM scheme. It exposes the three standard x402 endpoints —
// GET /supported, POST /verify, POST /settle — that resource servers and
// clients talk to.
//
// In the subscription model, this service is the neutral relayer:
//   - It submits the onchain `deposit` when a subscriber opens a channel.
//   - It verifies each off-chain voucher (a signature check, no chain call).
//   - It submits batched `claimWithSignature` + `settle` transactions when the
//     resource server decides to sweep accumulated usage onchain.
//   - It submits `refundWithSignature` when a subscriber cancels and reclaims
//     their unused prepaid balance.
//
// The facilitator holds no custody: subscribers' funds live in the onchain
// escrow contract, and every state transition is authorized by an EIP-712
// signature from the payer or the receiver's authorizer.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/joho/godotenv"
	x402 "github.com/x402-foundation/x402/go/v2"
	batchsettlement "github.com/x402-foundation/x402/go/v2/mechanisms/evm/batch-settlement"
	batchedfac "github.com/x402-foundation/x402/go/v2/mechanisms/evm/batch-settlement/facilitator"
	exactfac "github.com/x402-foundation/x402/go/v2/mechanisms/evm/exact/facilitator"
	uptofac "github.com/x402-foundation/x402/go/v2/mechanisms/evm/upto/facilitator"
)

const defaultPort = "4022"

func main() {
	_ = godotenv.Load()

	port := envOr("PORT", defaultPort)

	evmPrivateKey := os.Getenv("EVM_PRIVATE_KEY")
	if evmPrivateKey == "" {
		fmt.Println("EVM_PRIVATE_KEY environment variable is required")
		os.Exit(1)
	}

	// Networks come from the built-in registry (networks.go) — each has default
	// public RPCs, so nothing needs configuring. NETWORKS selects a subset
	// ("all", "testnets", "mainnets", or comma-separated names) and
	// RPC_URL_<NAME> overrides a chain's endpoint.
	chains, err := selectChains(os.Getenv("NETWORKS"))
	if err != nil {
		fmt.Printf("Invalid NETWORKS: %v\n", err)
		os.Exit(1)
	}

	// The receiverAuthorizer signs ClaimBatch / Refund EIP-712 messages and is
	// advertised under /supported so servers can delegate to it. If a dedicated
	// key isn't supplied, the facilitator advertises no authorizer and servers
	// must run their own (the recommended production path — see the READMEs).
	var authorizer batchsettlement.AuthorizerSigner
	if authKey := os.Getenv("EVM_RECEIVER_AUTHORIZER_PRIVATE_KEY"); authKey != "" {
		authorizer, err = newAuthorizerSigner(authKey)
		if err != nil {
			fmt.Printf("Failed to create authorizer signer: %v\n", err)
			os.Exit(1)
		}
	}

	if addr, err := addressFromPrivateKey(evmPrivateKey); err == nil {
		fmt.Printf("EVM Facilitator account: %s\n", addr)
	}
	if authorizer != nil {
		fmt.Printf("EVM Receiver Authorizer: %s\n", authorizer.Address())
	} else {
		fmt.Println("EVM Receiver Authorizer: not configured (servers must self-manage)")
	}

	// One signer per chain: each dials its own RPC, verifies the chain ID, and
	// confirms the escrow contract is deployed. Unreachable chains are skipped
	// with a warning so one flaky public RPC can't take the facilitator down.
	fmt.Println("Networks:")
	facilitator := x402.Newx402Facilitator()
	connectCtx, connectCancel := context.WithTimeout(context.Background(), 60*time.Second)
	active := 0
	signers := map[x402.Network]*facilitatorEvmSigner{} // for stats token-metadata lookups
	for _, chain := range chains {
		signer, rpcURL, err := connectChain(connectCtx, chain, evmPrivateKey)
		if err != nil {
			fmt.Printf("  ✗ %-13s skipped: %v\n", chain.Name, err)
			continue
		}
		signers[chain.Network()] = signer
		// All three standard EVM schemes share the same signer:
		//   exact            — one-shot EIP-3009 transferWithAuthorization
		//   upto             — Permit2 max-authorization, charge actual usage
		//   batch-settlement — prepaid channel + off-chain vouchers (subscriptions)
		networks := []x402.Network{chain.Network()}
		facilitator.Register(networks, exactfac.NewExactEvmScheme(signer, nil))
		facilitator.Register(networks, uptofac.NewUptoEvmScheme(signer, nil))
		facilitator.Register(networks, batchedfac.NewBatchSettlementEvmScheme(signer, authorizer))
		fmt.Printf("  ✓ %-13s %s (%s)\n", chain.Name, chain.Network(), rpcURL)
		active++
	}
	connectCancel()
	if active == 0 {
		fmt.Println("No networks available — check connectivity or RPC_URL_* overrides")
		os.Exit(1)
	}

	// Observability hooks — swap for structured logging / metrics in production.
	facilitator.OnAfterVerify(func(ctx x402.FacilitatorVerifyResultContext) error {
		fmt.Printf("[verify] ok\n")
		return nil
	})
	stats := loadStats(envOr("STATS_FILE", "data/stats.json"))
	tokenMetas := newTokenMetaCache()
	facilitator.OnAfterSettle(func(ctx x402.FacilitatorSettleResultContext) error {
		fmt.Printf("[settle] tx=%s\n", ctx.Result.Transaction)
		// Index onchain settlements only: batch-settlement vouchers settle with
		// an empty tx (pure off-chain accounting). Amounts come from the receipt
		// so the live feed and the explorer backfill share one definition.
		if !ctx.Result.Success || ctx.Result.Transaction == "" {
			return nil
		}
		signer := signers[ctx.Result.Network]
		if signer == nil {
			return nil
		}
		if err := indexTx(ctx.Ctx, stats, tokenMetas, signer, ctx.Result.Network, ctx.Result.Transaction, 0); err != nil {
			fmt.Printf("[index] live: %v\n", err)
			return nil
		}
		stats.save()
		return nil
	})

	// Backfill history from the chain (facilitator address = tx sender) and
	// keep polling so nothing the live hook misses stays missing.
	backfillInterval := 10 * time.Minute
	if v, err := time.ParseDuration(os.Getenv("BACKFILL_INTERVAL")); err == nil && v > 0 {
		backfillInterval = v
	}
	if facAddr, err := addressFromPrivateKey(evmPrivateKey); err == nil {
		runIndexer(stats, tokenMetas, chains, signers, facAddr, backfillInterval)
	}

	mux := http.NewServeMux()

	// GET /health — cheap liveness probe (no capability payload; use /supported
	// for the full scheme × network list).
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// GET /stats — per network × asset settlement index: onchain tx count and
	// total settled amount in base units.
	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"networks": stats.snapshot()})
	})

	// GET /supported — advertises the schemes, networks, and (optionally) the
	// receiverAuthorizer address that clients and servers discover at startup.
	mux.HandleFunc("GET /supported", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, facilitator.GetSupported())
	})

	// POST /verify — cheap, off-chain validation of a payment payload against
	// its requirements. For batch-settlement this is a voucher signature check
	// plus a channel-state lookup; no transaction is submitted.
	mux.HandleFunc("POST /verify", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()

		payload, requirements, err := readVerifyBody(r)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		result, err := facilitator.Verify(ctx, payload, requirements)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": errorMessage(err)})
			return
		}
		writeJSON(w, http.StatusOK, result)
	})

	// POST /settle — submits the onchain transaction(s). For batch-settlement
	// the resource server drives this via its ChannelManager (deposit on first
	// request, then batched claim/settle, and refunds on cancellation).
	mux.HandleFunc("POST /settle", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()

		payload, requirements, err := readVerifyBody(r)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		result, err := facilitator.Settle(ctx, payload, requirements)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": errorMessage(err)})
			return
		}
		writeJSON(w, http.StatusOK, result)
	})

	fmt.Printf("Facilitator listening on http://localhost:%s\n", port)
	fmt.Println("  GET  /health")
	fmt.Println("  GET  /stats")
	fmt.Println("  GET  /supported")
	fmt.Println("  POST /verify")
	fmt.Println("  POST /settle")
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		fmt.Printf("Server error: %v\n", err)
		os.Exit(1)
	}
}

// errorMessage adds a placement hint to the SDK's opaque version-detection
// error ("invalid version: 0"), which in practice almost always means the
// caller put x402Version at the top level of the request body instead of
// inside paymentPayload.
func errorMessage(err error) string {
	msg := err.Error()
	if strings.Contains(msg, "invalid version: 0") {
		msg += " (hint: x402Version must be a field inside paymentPayload, not at the top level of the request body)"
	}
	return msg
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func readVerifyBody(r *http.Request) (json.RawMessage, json.RawMessage, error) {
	var body struct {
		PaymentPayload      json.RawMessage `json:"paymentPayload"`
		PaymentRequirements json.RawMessage `json:"paymentRequirements"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return nil, nil, fmt.Errorf("invalid JSON body: %w", err)
	}
	if len(body.PaymentPayload) == 0 || len(body.PaymentRequirements) == 0 {
		return nil, nil, fmt.Errorf("missing paymentPayload or paymentRequirements")
	}
	return body.PaymentPayload, body.PaymentRequirements, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
