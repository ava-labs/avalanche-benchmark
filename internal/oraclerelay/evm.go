package oraclerelay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ava-labs/avalanchego/ids"
	ethcommon "github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/common/hexutil"
)

// evmClient is a dependency-free JSON-RPC client for a subnet-evm chain. The
// house style prefers plain HTTP over pulling in ethclient; every method maps to
// exactly one eth_* call.
type evmClient struct {
	url  string
	http *http.Client
}

// newEVMClient builds the EVM RPC endpoint for a chain served by a node:
// <node-url>/ext/bc/<chainID>/rpc.
func newEVMClient(nodeURL string, chainID ids.ID) *evmClient {
	base := strings.TrimRight(nodeURL, "/")
	return &evmClient{
		url:  fmt.Sprintf("%s/ext/bc/%s/rpc", base, chainID),
		http: &http.Client{Timeout: 30 * time.Second},
	}
}

// wsEndpoint derives the WebSocket log-subscription endpoint for a chain served
// by a node: ws(s)://<host>:<port>/ext/bc/<chainID>/ws. The node URL is given in
// http(s) form (same as the RPC endpoint), so the scheme is mapped rather than
// required to be ws up front.
func wsEndpoint(nodeURL string, chainID ids.ID) (string, error) {
	parsed, err := url.Parse(strings.TrimRight(nodeURL, "/"))
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("oracle node URL %q is not a valid URL", nodeURL)
	}
	switch parsed.Scheme {
	case "http", "ws":
		parsed.Scheme = "ws"
	case "https", "wss":
		parsed.Scheme = "wss"
	default:
		return "", fmt.Errorf("oracle node URL %q must be http(s) or ws(s), got scheme %q", nodeURL, parsed.Scheme)
	}
	parsed.Path = fmt.Sprintf("/ext/bc/%s/ws", chainID)
	return parsed.String(), nil
}

type jsonRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type jsonRPCResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *jsonRPCError   `json:"error"`
}

func (c *evmClient) call(ctx context.Context, method string, params []any, result any) error {
	body, err := json.Marshal(jsonRPCRequest{JSONRPC: "2.0", ID: 1, Method: method, Params: params})
	if err != nil {
		return fmt.Errorf("encode %s request: %w", method, err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build %s request: %w", method, err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("%s to %s: %w", method, c.url, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%s to %s: HTTP %d", method, c.url, response.StatusCode)
	}
	var decoded jsonRPCResponse
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		return fmt.Errorf("decode %s response: %w", method, err)
	}
	if decoded.Error != nil {
		return fmt.Errorf("%s: RPC error %d: %s", method, decoded.Error.Code, decoded.Error.Message)
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal(decoded.Result, result); err != nil {
		return fmt.Errorf("decode %s result: %w", method, err)
	}
	return nil
}

func (c *evmClient) ChainID(ctx context.Context) (*big.Int, error) {
	var raw hexutil.Big
	if err := c.call(ctx, "eth_chainId", nil, &raw); err != nil {
		return nil, err
	}
	return (*big.Int)(&raw), nil
}

func (c *evmClient) PendingNonce(ctx context.Context, address ethcommon.Address) (uint64, error) {
	var raw hexutil.Uint64
	if err := c.call(ctx, "eth_getTransactionCount", []any{address.Hex(), "pending"}, &raw); err != nil {
		return 0, err
	}
	return uint64(raw), nil
}

// LatestNonce returns the account nonce at the latest mined block, ignoring any
// still-queued pool transactions.
func (c *evmClient) LatestNonce(ctx context.Context, address ethcommon.Address) (uint64, error) {
	var raw hexutil.Uint64
	if err := c.call(ctx, "eth_getTransactionCount", []any{address.Hex(), "latest"}, &raw); err != nil {
		return 0, err
	}
	return uint64(raw), nil
}

func (c *evmClient) GasPrice(ctx context.Context) (*big.Int, error) {
	var raw hexutil.Big
	if err := c.call(ctx, "eth_gasPrice", nil, &raw); err != nil {
		return nil, err
	}
	return (*big.Int)(&raw), nil
}

// CallContract executes a read-only eth_call against the latest block.
func (c *evmClient) CallContract(ctx context.Context, to ethcommon.Address, data []byte) ([]byte, error) {
	var raw hexutil.Bytes
	params := []any{
		map[string]any{"to": to.Hex(), "data": hexutil.Encode(data)},
		"latest",
	}
	if err := c.call(ctx, "eth_call", params, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func (c *evmClient) SendRawTransaction(ctx context.Context, raw []byte) (ethcommon.Hash, error) {
	var hash ethcommon.Hash
	if err := c.call(ctx, "eth_sendRawTransaction", []any{hexutil.Encode(raw)}, &hash); err != nil {
		return ethcommon.Hash{}, err
	}
	return hash, nil
}

// receipt carries only the fields the oracle processes act on.
type receipt struct {
	Status      uint64
	BlockNumber uint64
}

type rpcReceipt struct {
	Status      hexutil.Uint64 `json:"status"`
	BlockNumber hexutil.Uint64 `json:"blockNumber"`
}

// TransactionReceipt returns nil, nil when the transaction is not yet mined.
func (c *evmClient) TransactionReceipt(ctx context.Context, hash ethcommon.Hash) (*receipt, error) {
	var raw *rpcReceipt
	if err := c.call(ctx, "eth_getTransactionReceipt", []any{hash.Hex()}, &raw); err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, nil
	}
	return &receipt{Status: uint64(raw.Status), BlockNumber: uint64(raw.BlockNumber)}, nil
}

// WaitReceipt polls until the transaction is mined or ctx is cancelled.
func (c *evmClient) WaitReceipt(ctx context.Context, hash ethcommon.Hash) (*receipt, error) {
	for {
		r, err := c.TransactionReceipt(ctx, hash)
		if err != nil {
			return nil, err
		}
		if r != nil {
			return r, nil
		}
		// 50ms: the confirmer drains FIFO, so by the time an item is dequeued its
		// receipt is usually already available; a long poll here caps confirm
		// throughput (250ms capped it at ~4/s and backed up the whole pipeline).
		if err := sleep(ctx, 50*time.Millisecond); err != nil {
			return nil, err
		}
	}
}
