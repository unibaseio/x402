package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// USD pricing for /stats via CoinMarketCap. Tokens are resolved by contract
// address (symbols collide: "U", "XUSD" are not unique), the id mapping is
// cached for the process lifetime and quotes for priceTTL. Anything CMC does
// not list stays unpriced — the summary reports how many assets that is
// rather than pretending a memecoin is worth $1.
// ponytail: one process-local cache; share via the stats file or a KV if
// multiple instances ever need consistent prices.

type usdQuote struct {
	Price   *big.Rat
	At      time.Time // CMC last_updated; zero for a peg (policy, not observation)
	Source  string    // "coinmarketcap" | "peg"
	expires time.Time
}

// pegQuotes parses PRICE_PEG_USD — comma-separated token addresses valued at
// exactly $1.00 — into quotes that win over CoinMarketCap. For USD-pegged
// tokens CMC does not list: XUSD ("Wrapped USDC") carries 99% of the BSC
// settlement history and was reported unpriced, so totalUsd understated the
// chain by ~$14. Anything not shaped like an address is ignored.
func pegQuotes(spec string) map[string]usdQuote {
	out := map[string]usdQuote{}
	for _, a := range strings.Split(spec, ",") {
		a = strings.ToLower(strings.TrimSpace(a))
		if strings.HasPrefix(a, "0x") && len(a) == 42 {
			out[a] = usdQuote{Price: big.NewRat(1, 1), Source: "peg"}
		}
	}
	return out
}

type cmcPricer struct {
	apiKey  string
	baseURL string
	ttl     time.Duration
	client  *http.Client

	mu     sync.Mutex
	ids    map[string]int      // lowercased address -> CMC id (0 = known-unlisted)
	quotes map[string]usdQuote // lowercased address -> quote
}

func newCMCPricer(apiKey string, ttl time.Duration) *cmcPricer {
	return &cmcPricer{
		apiKey:  apiKey,
		baseURL: "https://pro-api.coinmarketcap.com",
		ttl:     ttl,
		client:  &http.Client{Timeout: 15 * time.Second},
		ids:     map[string]int{},
		quotes:  map[string]usdQuote{},
	}
}

func (p *cmcPricer) get(ctx context.Context, path string, q url.Values, out any) error {
	req, _ := http.NewRequestWithContext(ctx, "GET", p.baseURL+path+"?"+q.Encode(), nil)
	req.Header.Set("X-CMC_PRO_API_KEY", p.apiKey)
	req.Header.Set("Accept", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var env struct {
		Status struct {
			ErrorCode    int    `json:"error_code"`
			ErrorMessage string `json:"error_message"`
		} `json:"status"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("cmc %s: %w", path, err)
	}
	if env.Status.ErrorCode != 0 {
		return &cmcError{code: env.Status.ErrorCode, msg: env.Status.ErrorMessage}
	}
	return json.Unmarshal(env.Data, out)
}

type cmcError struct {
	code int
	msg  string
}

func (e *cmcError) Error() string { return fmt.Sprintf("cmc error %d: %s", e.code, e.msg) }

// resolveID maps a contract address to a CMC id, caching misses as 0 so an
// unlisted token costs one lookup per process, not one per /stats call.
func (p *cmcPricer) resolveID(ctx context.Context, addr string) (int, error) {
	p.mu.Lock()
	id, ok := p.ids[addr]
	p.mu.Unlock()
	if ok {
		return id, nil
	}
	var data map[string]struct {
		ID int `json:"id"`
	}
	err := p.get(ctx, "/v2/cryptocurrency/info", url.Values{"address": {addr}, "aux": {""}}, &data)
	if err != nil {
		var ce *cmcError
		// 400 = "Invalid value for address": not listed. Cache it.
		if asCMC(err, &ce) && ce.code == 400 {
			p.mu.Lock()
			p.ids[addr] = 0
			p.mu.Unlock()
			return 0, nil
		}
		return 0, err
	}
	for _, v := range data {
		id = v.ID
		break
	}
	p.mu.Lock()
	p.ids[addr] = id
	p.mu.Unlock()
	return id, nil
}

func asCMC(err error, target **cmcError) bool {
	ce, ok := err.(*cmcError)
	if ok {
		*target = ce
	}
	return ok
}

// Quotes returns USD quotes for the addresses it can price. Missing keys are
// unpriced (unlisted or a transient CMC failure — never a fabricated price).
func (p *cmcPricer) Quotes(ctx context.Context, addrs []string) map[string]usdQuote {
	out := map[string]usdQuote{}
	if p == nil {
		return out
	}
	now := time.Now()
	need := map[int]string{} // id -> addr
	for _, a := range addrs {
		a = strings.ToLower(a)
		p.mu.Lock()
		q, ok := p.quotes[a]
		p.mu.Unlock()
		if ok && now.Before(q.expires) {
			out[a] = q
			continue
		}
		id, err := p.resolveID(ctx, a)
		if err != nil {
			fmt.Printf("[prices] resolve %s: %v\n", a, err)
			continue
		}
		if id != 0 {
			need[id] = a
		}
	}
	if len(need) == 0 {
		return out
	}
	ids := make([]string, 0, len(need))
	for id := range need {
		ids = append(ids, strconv.Itoa(id))
	}
	var data map[string]struct {
		Quote struct {
			USD struct {
				Price       float64   `json:"price"`
				LastUpdated time.Time `json:"last_updated"`
			} `json:"USD"`
		} `json:"quote"`
	}
	if err := p.get(ctx, "/v2/cryptocurrency/quotes/latest", url.Values{"id": {strings.Join(ids, ",")}, "convert": {"USD"}}, &data); err != nil {
		fmt.Printf("[prices] quotes: %v\n", err)
		return out
	}
	for idStr, v := range data {
		id, _ := strconv.Atoi(idStr)
		addr, ok := need[id]
		if !ok || v.Quote.USD.Price <= 0 {
			continue
		}
		price, _ := new(big.Rat).SetString(strconv.FormatFloat(v.Quote.USD.Price, 'f', -1, 64))
		q := usdQuote{Price: price, At: v.Quote.USD.LastUpdated, Source: "coinmarketcap", expires: now.Add(p.ttl)}
		p.mu.Lock()
		p.quotes[addr] = q
		p.mu.Unlock()
		out[addr] = q
	}
	return out
}
