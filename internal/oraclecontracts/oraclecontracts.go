// Package oraclecontracts embeds the DEPLOYED bytecode of the price-feed
// contracts so `l1 create` can bake them into Genesis allocs without solc or
// forge on the creation machine. The sources live in contracts/; regenerate
// the embedded artifacts with `forge inspect <Contract> deployedBytecode`
// as described in contracts/README.md. The contracts have no constructors or
// immutables — every configuration value is an explicit Genesis storage slot.
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

	AggregatorRuntime = strings.TrimSpace(aggregatorRuntime)
	ReceiverRuntime   = strings.TrimSpace(receiverRuntime)
)
