// Throwaway e2e: prove the local facilitator handles the `exact` scheme —
// server charges $0.001 per call via EIP-3009, client pays, facilitator
// verifies + settles onchain.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	x402 "github.com/x402-foundation/x402/go/v2"
	x402http "github.com/x402-foundation/x402/go/v2/http"
	nethttpmw "github.com/x402-foundation/x402/go/v2/http/nethttp"
	exactclient "github.com/x402-foundation/x402/go/v2/mechanisms/evm/exact/client"
	exactserver "github.com/x402-foundation/x402/go/v2/mechanisms/evm/exact/server"
	evmsigners "github.com/x402-foundation/x402/go/v2/signers/evm"
)

// NETWORK selects the chain (default Base Sepolia). On chains where the SDK
// has no default asset, set ASSET (+ ASSET_NAME / ASSET_VERSION for the
// EIP-712 domain) and the price is PRICE_UNITS base units of that token.
var network = x402.Network(envOr("NETWORK", "eip155:84532"))

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func price() x402.Price {
	asset := os.Getenv("ASSET")
	if asset == "" {
		return "$0.001"
	}
	// The exact server scheme accepts explicit assets as a map, not AssetAmount.
	return map[string]interface{}{
		"asset":  asset,
		"amount": envOr("PRICE_UNITS", "1000"),
		"extra":  map[string]interface{}{"name": envOr("ASSET_NAME", "TestUSD"), "version": envOr("ASSET_VERSION", "1")},
	}
}

func main() {
	payerKey := os.Getenv("PAYER_KEY")
	receiver := os.Getenv("RECEIVER")
	facilitatorURL := os.Getenv("FACILITATOR_URL")

	// --- server on :4026, exact scheme, $0.001/call ---
	fac := x402http.NewHTTPFacilitatorClient(&x402http.FacilitatorConfig{URL: facilitatorURL, Timeout: 120 * time.Second})
	routes := x402http.RoutesConfig{
		"GET /ping": {
			Accepts: x402http.PaymentOptions{
				{Scheme: "exact", Price: price(), Network: network, PayTo: receiver},
			},
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ping", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"pong": "exact"})
	})
	handler := nethttpmw.X402Payment(nethttpmw.Config{
		Routes:      routes,
		Facilitator: fac,
		Schemes:     []nethttpmw.SchemeConfig{{Network: network, Server: exactserver.NewExactEvmScheme()}},
		Timeout:     90 * time.Second,
	})(mux)
	srv := &http.Server{Addr: ":4026", Handler: handler}
	go func() { _ = srv.ListenAndServe() }()
	time.Sleep(500 * time.Millisecond)

	// --- client pays with exact ---
	signer, err := evmsigners.NewClientSignerFromPrivateKey(payerKey)
	if err != nil {
		fmt.Println("signer:", err)
		os.Exit(1)
	}
	xc := x402.Newx402Client()
	xc.Register("eip155:*", exactclient.NewExactEvmScheme(signer, nil))
	httpClient := x402http.WrapHTTPClientWithPayment(http.DefaultClient, x402http.Newx402HTTPClient(xc))

	t0 := time.Now()
	req, _ := http.NewRequestWithContext(context.Background(), "GET", "http://localhost:4026/ping", nil)
	resp, err := httpClient.Do(req)
	if err != nil {
		fmt.Println("request:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	var body any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	fmt.Printf("exact payment: %s (%.1fs) body=%v\n", resp.Status, time.Since(t0).Seconds(), body)

	if h := resp.Header.Get("PAYMENT-RESPONSE"); h != "" {
		if raw, err := base64.StdEncoding.DecodeString(h); err == nil {
			fmt.Printf("settle: %s\n", raw)
		}
	} else {
		fmt.Println("no PAYMENT-RESPONSE — settlement did not happen")
		os.Exit(1)
	}
}
