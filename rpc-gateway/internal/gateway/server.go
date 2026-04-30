package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/ava-labs/avalanche-benchmark/rpc-gateway/internal/config"
	"github.com/ava-labs/avalanche-benchmark/rpc-gateway/internal/policy"
	"github.com/ava-labs/avalanche-benchmark/rpc-gateway/internal/rpcjson"
	"github.com/ava-labs/avalanche-benchmark/rpc-gateway/internal/store"
)

type Server struct {
	logger          *slog.Logger
	cfg             config.Config
	store           *store.Postgres
	httpClient      *http.Client
	upstreamChainID string
	limiter         *fixedWindowLimiter
}

func New(ctx context.Context, logger *slog.Logger, cfg config.Config, db *store.Postgres) (*Server, error) {
	if err := db.Ping(ctx); err != nil {
		return nil, fmt.Errorf("failed to ping Postgres: %w", err)
	}

	httpClient := &http.Client{
		Timeout: cfg.RequestTimeout,
	}

	chainID, err := fetchUpstreamChainID(ctx, httpClient, cfg.UpstreamRPCURL)
	if err != nil {
		return nil, fmt.Errorf("failed to detect upstream chain ID: %w", err)
	}

	return &Server{
		logger:          logger,
		cfg:             cfg,
		store:           db,
		httpClient:      httpClient,
		upstreamChainID: chainID,
		limiter:         newFixedWindowLimiter(),
	}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/readyz", s.handleReadyz)
	mux.HandleFunc("/rpc", s.handleRPC)
	return mux
}

func (s *Server) UpstreamChainID() string {
	return s.upstreamChainID
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	if err := s.store.Ping(ctx); err != nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready"))
}

func (s *Server) handleRPC(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, s.cfg.MaxRequestBodyBytes))
	if err != nil {
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}

	req, err := rpcjson.DecodeSingleRequest(body)
	if err != nil {
		if err == rpcjson.ErrBatchRequestsNotSupported {
			rpcjson.WriteError(w, nil, rpcjson.CodeBatchUnsupported, "batch requests are not supported in this PoC")
			return
		}
		if err == rpcjson.ErrInvalidRequest {
			rpcjson.WriteError(w, nil, rpcjson.CodeInvalidRequest, "invalid JSON-RPC request")
			return
		}

		rpcjson.WriteErrorStatus(w, nil, rpcjson.CodeParseError, "invalid JSON-RPC payload", http.StatusBadRequest)
		return
	}

	clientIP := resolveClientIP(r, s.cfg.TrustForwardedFor)
	apiKey := extractAPIKey(r)
	if apiKey == "" {
		s.logDecision(req.Method, clientIP, "", "", nil, "deny", "missing API key")
		rpcjson.WriteError(w, req.ID, rpcjson.CodeAuthFailed, "missing API key")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestTimeout)
	defer cancel()

	access, err := s.store.LookupAPIKey(ctx, apiKey)
	if err != nil {
		if err == store.ErrNotFound {
			s.logDecision(req.Method, clientIP, "", "", nil, "deny", "invalid API key")
			rpcjson.WriteError(w, req.ID, rpcjson.CodeAuthFailed, "invalid API key")
			return
		}

		s.logger.Error("database lookup failed", "error", err)
		rpcjson.WriteError(w, req.ID, rpcjson.CodeInternalError, "policy lookup failed")
		return
	}

	if err := access.Policy.CheckSourceIP(clientIP); err != nil {
		s.logDecision(req.Method, clientIP, access.TenantID, access.APIKeyID, nil, "deny", err.Error())
		rpcjson.WriteError(w, req.ID, rpcjson.CodePolicyDenied, err.Error())
		return
	}

	if err := access.Policy.CheckMethod(req.Method); err != nil {
		s.logDecision(req.Method, clientIP, access.TenantID, access.APIKeyID, nil, "deny", err.Error())
		rpcjson.WriteError(w, req.ID, rpcjson.CodePolicyDenied, err.Error())
		return
	}

	if !s.limiter.Allow(access.APIKeyID, access.Policy.RequestsPerMinute(), time.Now()) {
		s.logDecision(req.Method, clientIP, access.TenantID, access.APIKeyID, nil, "deny", "rate limit exceeded")
		rpcjson.WriteError(w, req.ID, rpcjson.CodePolicyDenied, "rate limit exceeded")
		return
	}

	var attrs any

	switch req.Method {
	case "eth_sendRawTransaction":
		txAttrs, err := rpcjson.ExtractRawTransaction(req.Params)
		if err != nil {
			s.logDecision(req.Method, clientIP, access.TenantID, access.APIKeyID, nil, "deny", err.Error())
			rpcjson.WriteError(w, req.ID, rpcjson.CodeInvalidParams, err.Error())
			return
		}

		attrs = txAttrs

		if txAttrs.ChainID != s.upstreamChainID {
			reason := fmt.Sprintf("transaction chain ID %s does not match upstream chain ID %s", txAttrs.ChainID, s.upstreamChainID)
			s.logDecision(req.Method, clientIP, access.TenantID, access.APIKeyID, txAttrs, "deny", reason)
			rpcjson.WriteError(w, req.ID, rpcjson.CodePolicyDenied, reason)
			return
		}

		if err := access.Policy.CheckTransaction(txAttrs); err != nil {
			s.logDecision(req.Method, clientIP, access.TenantID, access.APIKeyID, txAttrs, "deny", err.Error())
			rpcjson.WriteError(w, req.ID, rpcjson.CodePolicyDenied, err.Error())
			return
		}
	case "eth_call", "eth_estimateGas":
		callAttrs, err := rpcjson.ExtractCall(req.Params)
		if err != nil {
			s.logDecision(req.Method, clientIP, access.TenantID, access.APIKeyID, nil, "deny", err.Error())
			rpcjson.WriteError(w, req.ID, rpcjson.CodeInvalidParams, err.Error())
			return
		}

		attrs = callAttrs

		if err := access.Policy.CheckCall(callAttrs); err != nil {
			s.logDecision(req.Method, clientIP, access.TenantID, access.APIKeyID, callAttrs, "deny", err.Error())
			rpcjson.WriteError(w, req.ID, rpcjson.CodePolicyDenied, err.Error())
			return
		}
	}

	resp, err := s.forwardRPC(ctx, body)
	if err != nil {
		s.logger.Error("upstream RPC request failed", "error", err, "method", req.Method, "tenant_id", access.TenantID)
		rpcjson.WriteError(w, req.ID, rpcjson.CodeUpstreamUnavailable, "upstream RPC unavailable")
		return
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		s.logger.Error("failed to read upstream response", "error", err)
		rpcjson.WriteError(w, req.ID, rpcjson.CodeUpstreamUnavailable, "failed to read upstream response")
		return
	}

	for key, values := range resp.Header {
		if strings.EqualFold(key, "Content-Length") {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}

	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(respBody)

	s.logDecision(req.Method, clientIP, access.TenantID, access.APIKeyID, attrs, "allow", "")
}

func (s *Server) forwardRPC(ctx context.Context, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.UpstreamRPCURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	return s.httpClient.Do(req)
}

func (s *Server) logDecision(method string, clientIP net.IP, tenantID, apiKeyID string, attrs any, decision, reason string) {
	fields := []any{
		"method", method,
		"client_ip", ipString(clientIP),
		"tenant_id", tenantID,
		"api_key_id", apiKeyID,
		"decision", decision,
	}

	if reason != "" {
		fields = append(fields, "reason", reason)
	}

	switch value := attrs.(type) {
	case policy.TransactionAttributes:
		fields = append(
			fields,
			"chain_id", value.ChainID,
			"from", value.From,
			"to", value.To,
			"selector", value.Selector,
			"gas_limit", value.GasLimit,
			"value_wei", value.Value.String(),
			"contract_creation", value.ContractCreation,
		)
	case policy.CallAttributes:
		fields = append(
			fields,
			"from", value.From,
			"to", value.To,
			"selector", value.Selector,
			"gas_limit", value.GasLimit,
			"value_wei", value.Value.String(),
		)
	}

	s.logger.Info("rpc policy decision", fields...)
}

func fetchUpstreamChainID(ctx context.Context, httpClient *http.Client, upstreamURL string) (string, error) {
	payload := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var chainResp struct {
		Result string `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&chainResp); err != nil {
		return "", err
	}
	if chainResp.Error != nil {
		return "", fmt.Errorf("upstream returned error: %s", chainResp.Error.Message)
	}

	return policy.NormalizeChainID(chainResp.Result)
}

func extractAPIKey(r *http.Request) string {
	if raw := strings.TrimSpace(r.Header.Get("X-API-Key")); raw != "" {
		return raw
	}

	authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
	if authHeader == "" {
		return ""
	}

	const bearerPrefix = "Bearer "
	if strings.HasPrefix(authHeader, bearerPrefix) {
		return strings.TrimSpace(strings.TrimPrefix(authHeader, bearerPrefix))
	}
	return ""
}

func resolveClientIP(r *http.Request, trustForwardedFor bool) net.IP {
	if trustForwardedFor {
		if forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); forwarded != "" {
			parts := strings.Split(forwarded, ",")
			if len(parts) > 0 {
				if ip := net.ParseIP(strings.TrimSpace(parts[0])); ip != nil {
					return ip
				}
			}
		}
	}

	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err == nil {
		if ip := net.ParseIP(host); ip != nil {
			return ip
		}
	}

	return net.ParseIP(strings.TrimSpace(r.RemoteAddr))
}

func ipString(ip net.IP) string {
	if ip == nil {
		return ""
	}
	return ip.String()
}
