# x402 v2 Subscription-like Payments (Go)

A complete, runnable **x402 v2** example in Go — **facilitator + resource server + client** —
that implements **subscription-like payments** with the **batch-settlement** EVM scheme.

Built on the official [`x402-foundation/x402`](https://github.com/x402-foundation/x402) Go SDK
(`github.com/x402-foundation/x402/go/v2`).

---

## Why batch-settlement = "subscription-like"

The default x402 `exact` scheme is pay-per-request: one signed authorization → one onchain
transaction per call. That's fine for the occasional purchase, but expensive and slow for a
metered API an agent hits hundreds of times.

**Batch-settlement** turns that into a prepaid subscription via a **unidirectional payment
channel**:

| Phase | What happens | Onchain? |
|-------|--------------|----------|
| **Subscribe** | Subscriber deposits a prepaid balance into an escrow contract (auto, on the 1st request). | ✅ one deposit tx |
| **Use** | Each request bumps a **cumulative off-chain voucher** (an EIP-712 signature) sent in the payment header. | ❌ signature only |
| **Bill** | The server periodically **claims** accumulated vouchers and **settles** them to itself in batches. | ✅ batched, amortized |
| **Cancel** | Cooperative **refund** returns the unused balance to the subscriber. | ✅ one refund tx |

So the subscriber pays gas ~once (the deposit is even gas-sponsored by the facilitator), then
gets unlimited sub-second, gas-free calls billed against their prepaid balance — exactly the
economics of a prepaid subscription. It also supports **usage-based metering**: the subscriber
authorizes a per-call *maximum*, and the server bills only what was actually consumed.

```
   SUBSCRIBER (client)                RESOURCE SERVER                 FACILITATOR
        │                                   │                              │
        │  GET /v1/insights                 │                              │
        │──────────────────────────────────▶│  402 + PaymentRequirements   │
        │◀──────────────────────────────────│                              │
        │  (1st time) sign deposit + voucher │                              │
        │──────────────────────────────────▶│  POST /verify (voucher sig)  │
        │                                    │─────────────────────────────▶│
        │                                    │  POST /settle → deposit tx   │
        │                                    │─────────────────────────────▶│──▶ chain
        │◀───────────── 200 + data ──────────│                              │
        │                                    │                              │
        │  more requests = new vouchers only │  ChannelManager batches      │
        │  (no chain, no gas, ~instant)      │  claim + settle periodically │─▶ chain
        │                                    │                              │
        │  scheme.Refund() to cancel         │  refundWithSignature         │─▶ chain
```

---

## Layout

```
unibase-x402/
├── go.work                  # ties the three modules together for local dev
├── facilitator/             # x402 v2 facilitator service (GET /supported, POST /verify, POST /settle)
│   ├── main.go              #   HTTP endpoints + scheme registration
│   ├── evm_signer.go        #   submits deposit/claim/settle/refund txs (implements FacilitatorEvmSigner)
│   └── authorizer.go        #   signs ClaimBatch/Refund EIP-712 messages (optional)
├── server/                  # resource server selling a metered "Unibase Pro" API
│   ├── main.go              #   route config + ChannelManager (auto claim/settle/refund)
│   └── signer.go            #   self-managed receiver authorizer
└── client/                  # subscriber: deposit once, many voucher-paid calls, optional refund
    └── main.go
```

Each directory is its own Go module and requires `github.com/x402-foundation/x402/go/v2`
(pinned to `v2.18.0`). The `go.work` file lets you build them together locally.

---

## Prerequisites

- **Go 1.24+**
- Two funded **Base Sepolia** test wallets (private keys):
  - **Facilitator wallet** — needs a little **ETH** for gas (it submits every onchain tx).
  - **Subscriber wallet** — needs **USDC** (the prepaid balance). Deposits are gas-sponsored,
    so it does *not* need ETH.
- A receiver address for the server (can be any address you control).
- Base Sepolia RPC (default `https://sepolia.base.org`) and testnet USDC
  (Circle faucet: <https://faucet.circle.com>).

> The batch-settlement contract must be deployed on the target network. It ships on
> **Base Sepolia (`eip155:84532`)** and **Base Mainnet (`eip155:8453`)**.

---

## Quick start (3 terminals)

### 1. Facilitator

```bash
cd facilitator
cp .env.example .env
#   EVM_PRIVATE_KEY = facilitator wallet (needs gas token on each enabled chain)
go run .
# → Facilitator listening on http://localhost:4022
```

Or with Docker (from the repo root):

```bash
EVM_PRIVATE_KEY=0x... docker compose up -d facilitator
```

Multi-chain is **built in** — Base Sepolia, Base, BSC testnet, BSC, Polygon and Arbitrum all
work with zero RPC configuration (the batch-settlement escrow is CREATE2-deployed at the same
address on every chain). Select a subset with `NETWORKS=testnets|mainnets|base-sepolia,bsc`,
override an endpoint with `RPC_URL_<NAME>` (e.g. `RPC_URL_BSC=...`).

### 2. Resource server (subscription API)

```bash
cd server
cp .env.example .env
#   EVM_ADDRESS   = receiver address (where revenue settles)
#   FACILITATOR_URL = http://localhost:4022
#   EVM_RECEIVER_AUTHORIZER_PRIVATE_KEY = a key you control (recommended)
go run .
# → Unibase Pro subscription API listening at http://localhost:4021
#     GET /v1/health   (free)
#     GET /v1/insights (metered, up to $0.01/call)
```

### 3. Subscriber client

```bash
cd client
cp .env.example .env
#   EVM_PRIVATE_KEY = subscriber wallet (needs USDC)
#   NUMBER_OF_REQUESTS=5
go run .
```

You'll see the first request open the channel (deposit + voucher) and the rest settle as pure
off-chain vouchers in well under a second each. The server logs `[claim]` / `[settle]` as its
`ChannelManager` folds the vouchers onchain in batches.

To simulate **cancelling** and reclaiming the unused balance, set `REFUND_AFTER_REQUESTS=true`
(and optionally `REFUND_AMOUNT` for a partial refund) in `client/.env`.

---

## The facilitator API

The facilitator exposes the three standard x402 v2 endpoints (spec §7) and holds **no
custody** — funds move straight from payer to receiver (or live in the escrow contract for
batch-settlement); every transition is authorized by the payer's signature.

| Endpoint | Purpose |
|----------|---------|
| `GET /health` | Cheap liveness probe (`{"status":"ok"}`). |
| `GET /supported` | Advertises schemes, networks, and (optional) `receiverAuthorizer`. |
| `POST /verify` | Off-chain payload validation (signature / balance / channel state). No tx. |
| `POST /settle` | Submits the onchain tx. |

(`GET /discovery/*` from spec §8 is the optional Bazaar marketplace extension — not implemented.)

All three standard EVM schemes are registered on every enabled network:

| Scheme | Model | Mechanism |
|--------|-------|-----------|
| `exact` | Fixed price per call | EIP-3009 `transferWithAuthorization` |
| `upto` | Authorize a max, charge actual usage | Permit2 |
| `batch-settlement` | Prepaid channel + off-chain vouchers (subscriptions) | Escrow contract + EIP-712 vouchers |

Two keys drive it:
- **`EVM_PRIVATE_KEY`** — the wallet that signs and broadcasts transactions (pays gas).
- **`EVM_RECEIVER_AUTHORIZER_PRIVATE_KEY`** *(optional)* — signs `ClaimBatch`/`Refund` messages
  advertised under `/supported`. Omit it to require servers to run their own authorizer
  (recommended: the server's channels then survive a facilitator change).

---

## Using Coinbase CDP's hosted facilitator instead

You don't have to run the Go facilitator — the server/client just need a `FACILITATOR_URL`.
The SDK's `HTTPFacilitatorClient` points at any URL and supports auth headers via an
`AuthProvider`:

```go
import x402http "github.com/x402-foundation/x402/go/v2/http"

facilitator := x402http.NewHTTPFacilitatorClient(&x402http.FacilitatorConfig{
    URL:          "https://api.cdp.coinbase.com/platform/v2/x402", // CDP v2 base
    AuthProvider: myCdpAuthProvider,                               // supplies CDP API-key headers
})
```

- **Testnet, no key:** the public `https://x402.org/facilitator` works for standard schemes.
- **Production:** CDP (`/v2/x402/verify`, `/v2/x402/settle`) with a CDP API key via `AuthProvider`.

> ⚠️ Batch-settlement is a newer scheme. Before relying on a hosted facilitator for it, check
> that its `GET /supported` actually advertises the `batched` scheme for your network. Running
> the Go facilitator in this repo guarantees batch-settlement support end-to-end.

---

## Environment reference

<details>
<summary><b>facilitator/.env</b></summary>

| Variable | Required | Description |
|----------|----------|-------------|
| `EVM_PRIVATE_KEY` | ✅ | Facilitator wallet; signs & submits txs (needs ETH for gas). |
| `EVM_RPC_URL` | | Default `https://sepolia.base.org`. |
| `EVM_RECEIVER_AUTHORIZER_PRIVATE_KEY` | | Advertise this authorizer under `/supported`. |
| `PORT` | | Listen port (default `4022`). |
</details>

<details>
<summary><b>server/.env</b></summary>

| Variable | Required | Description |
|----------|----------|-------------|
| `EVM_ADDRESS` | ✅ | Receiver (`payTo`) address — where revenue settles. |
| `FACILITATOR_URL` | ✅ | e.g. `http://localhost:4022`. |
| `EVM_RECEIVER_AUTHORIZER_PRIVATE_KEY` | | Self-managed authorizer (recommended). Omit to delegate. |
| `STORAGE_DIR` | | Persist channel sessions across restarts. |
| `DEFERRED_WITHDRAW_DELAY_SECONDS` | | Channel challenge window; default `86400`. |
</details>

<details>
<summary><b>client/.env</b></summary>

| Variable | Required | Description |
|----------|----------|-------------|
| `EVM_PRIVATE_KEY` | ✅ | Subscriber wallet; funds deposit & signs vouchers (needs USDC). |
| `EVM_VOUCHER_SIGNER_PRIVATE_KEY` | | Dedicated voucher key (recommended for smart wallets). |
| `EVM_RPC_URL` | | Cold-start channel-state recovery. |
| `RESOURCE_SERVER_URL` / `ENDPOINT_PATH` | | Default `http://localhost:4021` `/v1/insights`. |
| `CHANNEL_SALT` | | Differentiate channels with the same payer/payee/token. |
| `STORAGE_DIR` | | Persist session state (keeps vouchers cumulative). |
| `NUMBER_OF_REQUESTS` | | Metered calls per run (default `5`). |
| `DEPOSIT_MULTIPLIER` | | Prepaid balance = `maxPricePerCall × multiplier`. |
| `REFUND_AFTER_REQUESTS` / `REFUND_AMOUNT` | | Cancel & reclaim unused balance. |
</details>

---

## Production notes

- **Withdraw delay:** set `DEFERRED_WITHDRAW_DELAY_SECONDS` comfortably larger than your claim
  cadence plus an operational safety margin.
- **Self-managed authorizer:** have the server hold its own `receiverAuthorizer` key (ideally in
  a KMS/HSM — the local ECDSA signer in `server/signer.go` is a placeholder) so channels aren't
  bound to one facilitator.
- **Refund policy:** the demo refunds channels idle for 3 minutes. In production, drive refunds
  from an explicit "cancel subscription" event, not pure idleness.
- **Facilitator hardening:** the `facilitatorEvmSigner` uses naive nonce/gas handling and a fixed
  gas limit — add real nonce management, gas estimation, retries, and monitoring before mainnet.
- **Persistence:** set `STORAGE_DIR` on both server and client so subscriptions survive restarts.

## References

- Scheme spec — [`specs/schemes/batch-settlement/scheme_batch_settlement_evm.md`](https://github.com/x402-foundation/x402/blob/main/specs/schemes/batch-settlement/scheme_batch_settlement_evm.md)
- x402 v2 spec — [`specs/x402-specification-v2.md`](https://github.com/x402-foundation/x402/blob/main/specs/x402-specification-v2.md)
- Go SDK batch-settlement README — [`go/mechanisms/evm/batch-settlement`](https://github.com/x402-foundation/x402/tree/main/go/mechanisms/evm/batch-settlement)
- Coinbase CDP x402 facilitator — <https://docs.cdp.coinbase.com/api-reference/v2/rest-api/x402-facilitator>
