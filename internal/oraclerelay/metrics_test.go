package oraclerelay

import (
	"math"
	"math/big"
	"testing"
)

func TestScaledPrice(t *testing.T) {
	cases := []struct {
		raw  int64
		want float64
	}{
		{0, 0},
		{2500000000, 25},                // 25e8
		{6000012345678, 60000.12345678}, // 8-decimal fixed point
		{1, 0.00000001},
	}
	for _, tc := range cases {
		got := scaledPrice(big.NewInt(tc.raw))
		if math.Abs(got-tc.want) > 1e-9 {
			t.Fatalf("scaledPrice(%d) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

func TestAssetIDByName(t *testing.T) {
	if id, ok := assetIDByName("BTC-USD"); !ok || id != assetBTC {
		t.Fatalf("BTC-USD reverse lookup = (%s, %v)", id.Hex(), ok)
	}
	if id, ok := assetIDByName("USDC-USD"); !ok || id != assetUSDC {
		t.Fatalf("USDC-USD reverse lookup = (%s, %v)", id.Hex(), ok)
	}
	if _, ok := assetIDByName("ETH-USD"); ok {
		t.Fatal("unknown symbol must not resolve")
	}
}

func TestAssetNameReverseRoundTrip(t *testing.T) {
	for _, asset := range KnownAssets {
		id, ok := assetIDByName(asset.name)
		if !ok {
			t.Fatalf("known asset %s missing from reverse map", asset.name)
		}
		if name := AssetName(id); name != asset.name {
			t.Fatalf("round trip: AssetName(assetIDByName(%q)) = %q", asset.name, name)
		}
	}
}
