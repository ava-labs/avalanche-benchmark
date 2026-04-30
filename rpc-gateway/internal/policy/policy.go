package policy

import (
	"encoding/hex"
	"fmt"
	"math/big"
	"net"
	"strings"

	"github.com/ava-labs/libevm/common"
)

type Document struct {
	AllowedCIDRs             []string `json:"allowedCidrs"`
	AllowedMethods           []string `json:"allowedMethods"`
	AllowedChainIDs          []string `json:"allowedChainIds"`
	AllowedFromAddresses     []string `json:"allowedFromAddresses"`
	AllowedToAddresses       []string `json:"allowedToAddresses"`
	AllowedFunctionSelectors []string `json:"allowedFunctionSelectors"`
	AllowContractCreation    bool     `json:"allowContractCreation"`
	MaxGasLimit              uint64   `json:"maxGasLimit"`
	MaxValueWei              string   `json:"maxValueWei"`
	RequestsPerMinuteLimit   int      `json:"requestsPerMinute"`
}

type Compiled struct {
	requestsPerMinute int
	allowCreate       bool
	maxGasLimit       uint64
	maxValueWei       *big.Int
	allowedCIDRs      []*net.IPNet
	allowedMethods    map[string]struct{}
	allowedChainIDs   map[string]struct{}
	allowedFrom       map[string]struct{}
	allowedTo         map[string]struct{}
	allowedSelectors  map[string]struct{}
}

type TransactionAttributes struct {
	ChainID          string
	From             string
	To               string
	Selector         string
	GasLimit         uint64
	Value            *big.Int
	ContractCreation bool
}

type CallAttributes struct {
	From     string
	To       string
	Selector string
	GasLimit uint64
	Value    *big.Int
}

func Compile(doc Document) (*Compiled, error) {
	compiled := &Compiled{
		requestsPerMinute: doc.RequestsPerMinuteLimit,
		allowCreate:       doc.AllowContractCreation,
		maxGasLimit:       doc.MaxGasLimit,
		allowedMethods:    make(map[string]struct{}),
		allowedChainIDs:   make(map[string]struct{}),
		allowedFrom:       make(map[string]struct{}),
		allowedTo:         make(map[string]struct{}),
		allowedSelectors:  make(map[string]struct{}),
	}

	for _, raw := range doc.AllowedCIDRs {
		cidr, err := parseCIDROrIP(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid allowedCidrs entry %q: %w", raw, err)
		}
		compiled.allowedCIDRs = append(compiled.allowedCIDRs, cidr)
	}

	for _, method := range doc.AllowedMethods {
		method = strings.TrimSpace(method)
		if method != "" {
			compiled.allowedMethods[method] = struct{}{}
		}
	}

	for _, chainID := range doc.AllowedChainIDs {
		normalized, err := NormalizeChainID(chainID)
		if err != nil {
			return nil, fmt.Errorf("invalid allowedChainIds entry %q: %w", chainID, err)
		}
		compiled.allowedChainIDs[normalized] = struct{}{}
	}

	for _, address := range doc.AllowedFromAddresses {
		normalized, err := normalizeAddress(address)
		if err != nil {
			return nil, fmt.Errorf("invalid allowedFromAddresses entry %q: %w", address, err)
		}
		compiled.allowedFrom[normalized] = struct{}{}
	}

	for _, address := range doc.AllowedToAddresses {
		normalized, err := normalizeAddress(address)
		if err != nil {
			return nil, fmt.Errorf("invalid allowedToAddresses entry %q: %w", address, err)
		}
		compiled.allowedTo[normalized] = struct{}{}
	}

	for _, selector := range doc.AllowedFunctionSelectors {
		normalized, err := normalizeSelector(selector)
		if err != nil {
			return nil, fmt.Errorf("invalid allowedFunctionSelectors entry %q: %w", selector, err)
		}
		compiled.allowedSelectors[normalized] = struct{}{}
	}

	if strings.TrimSpace(doc.MaxValueWei) != "" {
		maxValueWei, ok := new(big.Int).SetString(strings.TrimSpace(doc.MaxValueWei), 10)
		if !ok {
			return nil, fmt.Errorf("invalid maxValueWei %q", doc.MaxValueWei)
		}
		compiled.maxValueWei = maxValueWei
	}

	return compiled, nil
}

func (c *Compiled) RequestsPerMinute() int {
	return c.requestsPerMinute
}

func (c *Compiled) CheckSourceIP(ip net.IP) error {
	if len(c.allowedCIDRs) == 0 || ip == nil {
		return nil
	}

	for _, cidr := range c.allowedCIDRs {
		if cidr.Contains(ip) {
			return nil
		}
	}

	return fmt.Errorf("source IP %s is not allowed", ip.String())
}

func (c *Compiled) CheckMethod(method string) error {
	if len(c.allowedMethods) == 0 {
		return nil
	}

	if _, ok := c.allowedMethods[method]; ok {
		return nil
	}

	return fmt.Errorf("RPC method %s is not allowed", method)
}

func (c *Compiled) CheckTransaction(tx TransactionAttributes) error {
	if err := c.checkChainID(tx.ChainID); err != nil {
		return err
	}
	if err := c.checkFrom(tx.From); err != nil {
		return err
	}
	if err := c.checkTo(tx.To, tx.ContractCreation); err != nil {
		return err
	}
	if err := c.checkSelector(tx.Selector); err != nil {
		return err
	}
	if err := c.checkGas(tx.GasLimit); err != nil {
		return err
	}
	if err := c.checkValue(tx.Value); err != nil {
		return err
	}
	return nil
}

func (c *Compiled) CheckCall(call CallAttributes) error {
	if call.From != "" {
		normalizedFrom, err := normalizeAddress(call.From)
		if err != nil {
			return err
		}
		if len(c.allowedFrom) > 0 {
			if _, ok := c.allowedFrom[normalizedFrom]; !ok {
				return fmt.Errorf("from address %s is not allowed", normalizedFrom)
			}
		}
	}

	if call.To != "" || len(c.allowedTo) > 0 {
		if err := c.checkTo(call.To, false); err != nil {
			return err
		}
	}

	if call.Selector != "" || len(c.allowedSelectors) > 0 {
		if err := c.checkSelector(call.Selector); err != nil {
			return err
		}
	}

	if err := c.checkGas(call.GasLimit); err != nil {
		return err
	}
	if err := c.checkValue(call.Value); err != nil {
		return err
	}
	return nil
}

func NormalizeChainID(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("chain ID is empty")
	}

	value := new(big.Int)
	var ok bool

	if strings.HasPrefix(strings.ToLower(raw), "0x") {
		value, ok = value.SetString(strings.TrimPrefix(strings.ToLower(raw), "0x"), 16)
	} else {
		value, ok = value.SetString(raw, 10)
	}
	if !ok {
		return "", fmt.Errorf("invalid chain ID")
	}

	return value.String(), nil
}

func (c *Compiled) checkChainID(chainID string) error {
	if len(c.allowedChainIDs) == 0 {
		return nil
	}

	normalized, err := NormalizeChainID(chainID)
	if err != nil {
		return err
	}

	if _, ok := c.allowedChainIDs[normalized]; ok {
		return nil
	}

	return fmt.Errorf("chain ID %s is not allowed", normalized)
}

func (c *Compiled) checkFrom(address string) error {
	if len(c.allowedFrom) == 0 {
		return nil
	}

	normalized, err := normalizeAddress(address)
	if err != nil {
		return err
	}

	if _, ok := c.allowedFrom[normalized]; ok {
		return nil
	}

	return fmt.Errorf("from address %s is not allowed", normalized)
}

func (c *Compiled) checkTo(address string, contractCreation bool) error {
	if contractCreation {
		if !c.allowCreate {
			return fmt.Errorf("contract creation is not allowed")
		}
		return nil
	}

	if len(c.allowedTo) == 0 {
		return nil
	}

	normalized, err := normalizeAddress(address)
	if err != nil {
		return err
	}

	if _, ok := c.allowedTo[normalized]; ok {
		return nil
	}

	return fmt.Errorf("to address %s is not allowed", normalized)
}

func (c *Compiled) checkSelector(selector string) error {
	if len(c.allowedSelectors) == 0 {
		return nil
	}

	normalized, err := normalizeSelector(selector)
	if err != nil {
		return err
	}

	if _, ok := c.allowedSelectors[normalized]; ok {
		return nil
	}

	return fmt.Errorf("function selector %s is not allowed", normalized)
}

func (c *Compiled) checkGas(gasLimit uint64) error {
	if c.maxGasLimit == 0 {
		return nil
	}
	if gasLimit <= c.maxGasLimit {
		return nil
	}
	return fmt.Errorf("gas limit %d exceeds allowed maximum %d", gasLimit, c.maxGasLimit)
}

func (c *Compiled) checkValue(value *big.Int) error {
	if c.maxValueWei == nil || value == nil {
		return nil
	}
	if value.Cmp(c.maxValueWei) <= 0 {
		return nil
	}
	return fmt.Errorf("value %s exceeds allowed maximum %s", value.String(), c.maxValueWei.String())
}

func normalizeAddress(raw string) (string, error) {
	if !common.IsHexAddress(raw) {
		return "", fmt.Errorf("invalid hex address")
	}
	return strings.ToLower(common.HexToAddress(raw).Hex()), nil
}

func normalizeSelector(raw string) (string, error) {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "" {
		return "", fmt.Errorf("selector is empty")
	}

	if !strings.HasPrefix(raw, "0x") {
		raw = "0x" + raw
	}
	if len(raw) != 10 {
		return "", fmt.Errorf("selector must be 4 bytes")
	}
	if _, err := hex.DecodeString(raw[2:]); err != nil {
		return "", fmt.Errorf("selector must be hex encoded")
	}
	return raw, nil
}

func parseCIDROrIP(raw string) (*net.IPNet, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("CIDR is empty")
	}

	if strings.Contains(raw, "/") {
		_, cidr, err := net.ParseCIDR(raw)
		return cidr, err
	}

	ip := net.ParseIP(raw)
	if ip == nil {
		return nil, fmt.Errorf("invalid IP address")
	}

	if ip.To4() != nil {
		return &net.IPNet{IP: ip, Mask: net.CIDRMask(32, 32)}, nil
	}
	return &net.IPNet{IP: ip, Mask: net.CIDRMask(128, 128)}, nil
}
