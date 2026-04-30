package rpcjson

import (
	"encoding/json"
	"errors"
	"net/http"
)

var ErrBatchRequestsNotSupported = errors.New("batch requests are not supported")
var ErrInvalidRequest = errors.New("invalid JSON-RPC request")

const (
	CodeParseError          = -32700
	CodeInvalidRequest      = -32600
	CodeInvalidParams       = -32602
	CodeInternalError       = -32603
	CodeAuthFailed          = -32001
	CodePolicyDenied        = -32002
	CodeBatchUnsupported    = -32003
	CodeUpstreamUnavailable = -32004
)

type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type errorEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Error   errorObject     `json:"error"`
}

type errorObject struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func DecodeSingleRequest(body []byte) (Request, error) {
	trimmed := trimLeadingWhitespace(body)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		return Request{}, ErrBatchRequestsNotSupported
	}

	var req Request
	if err := json.Unmarshal(body, &req); err != nil {
		return Request{}, err
	}

	if req.JSONRPC != "2.0" || req.Method == "" {
		return Request{}, ErrInvalidRequest
	}

	return req, nil
}

func WriteError(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	WriteErrorStatus(w, id, code, message, http.StatusOK)
}

func WriteErrorStatus(w http.ResponseWriter, id json.RawMessage, code int, message string, status int) {
	envelope := errorEnvelope{
		JSONRPC: "2.0",
		ID:      normalizeID(id),
		Error: errorObject{
			Code:    code,
			Message: message,
		},
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(envelope)
}

func normalizeID(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	return id
}

func trimLeadingWhitespace(body []byte) []byte {
	for len(body) > 0 {
		switch body[0] {
		case ' ', '\n', '\r', '\t':
			body = body[1:]
		default:
			return body
		}
	}
	return body
}
