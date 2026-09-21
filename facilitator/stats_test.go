package main

import "testing"

// Asset numbers are the 2026-09-21 production backfill, so this pins the
// rollup to something that was checked against the chain. Network TxCount is
// the facilitator's own tx count and may exceed the per-asset sum (claims
// move no tokens).
func TestSummarizeRollsUpPerNetwork(t *testing.T) {
	snap := map[string]*networkTotals{
		"bsc": {TxCount: 1814, Assets: map[string]*assetTotals{
			"0xf3a3": {Symbol: "XUSD", Decimals: 18, TxCount: 1796, TotalAmount: "13935168000000032400"},
			"0xce24": {Symbol: "U", Decimals: 18, TxCount: 15, TotalAmount: "14031000000000000000"},
			"0x0df0": {Symbol: "Cheems", Decimals: 18, TxCount: 1, TotalAmount: "10000000000000000000"},
			"0x8ac7": {Symbol: "USDC", Decimals: 18, TxCount: 1, TotalAmount: "100000000000000000"},
		}},
		"base-sepolia": {TxCount: 14, Assets: map[string]*assetTotals{
			"0x036c": {Symbol: "USDC", Decimals: 6, TxCount: 14, TotalAmount: "5658000"},
		}},
		"empty": {Assets: map[string]*assetTotals{}},
	}
	got := summarize(snap)
	if got["bsc"].TxCount != 1814 || got["bsc"].Assets != 4 || got["bsc"].TotalAmount != "38.0661680000000324" {
		t.Fatalf("bsc = %+v", got["bsc"])
	}
	if got["base-sepolia"].TxCount != 14 || got["base-sepolia"].TotalAmount != "5.658" {
		t.Fatalf("base-sepolia = %+v", got["base-sepolia"])
	}
	if got["empty"].TxCount != 0 || got["empty"].TotalAmount != "0" || got["empty"].Assets != 0 {
		t.Fatalf("empty = %+v", got["empty"])
	}
	// Mixed decimals on one network must be summed in token units, not base units.
	mixed := map[string]*networkTotals{"x": {TxCount: 2, Assets: map[string]*assetTotals{
		"a": {Decimals: 6, TxCount: 1, TotalAmount: "1500000"},
		"b": {Decimals: 18, TxCount: 1, TotalAmount: "2500000000000000000"},
	}}}
	if s := summarize(mixed)["x"]; s.TotalAmount != "4" || s.TxCount != 2 {
		t.Fatalf("mixed = %+v", s)
	}
}
