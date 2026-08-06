// Package oraclecontracts embeds the DEPLOYED bytecode of the price-feed
// contracts so `l1 create` can bake them into Genesis allocs without solc or
// forge on the creation machine. The sources live in the sibling contracts/
// directory; regenerate the embedded artifacts with `forge inspect <Contract>
// deployedBytecode` as described in its README. The contracts have no
// constructors or immutables: every configuration value is an explicit
// Genesis storage slot.
//
// This package is deliberately NOT under the app's internal/ tree: baking
// happens in the base layer's `l1 create`, and this import is the one
// explicit base-to-app edge, to be replaced by a genesis-patch drop-in when
// apps grow a real genesis hook.
package oraclecontracts

import (
	_ "embed"
	"strings"
)

var (
	//go:embed PriceFeedAggregator.runtime.hex
	aggregatorRuntime string
	//go:embed PriceFeedReceiver.runtime.hex
	receiverRuntime string
	//go:embed PriceAggregator.runtime.hex
	priceAggregatorRuntime string
	//go:embed PriceFeedProxy.runtime.hex
	priceFeedProxyRuntime string

	AggregatorRuntime      = strings.TrimSpace(aggregatorRuntime)
	ReceiverRuntime        = strings.TrimSpace(receiverRuntime)
	PriceAggregatorRuntime = strings.TrimSpace(priceAggregatorRuntime)
	PriceFeedProxyRuntime  = strings.TrimSpace(priceFeedProxyRuntime)
)
