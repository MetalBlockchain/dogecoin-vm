package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

// rpcClient is a minimal JSON-RPC 1.0 client. It speaks to both btcd (the
// DogecoinVM node) and Dogecoin Core, whose APIs differ in places that
// btcd's rpcclient papers over wrongly.
type rpcClient struct {
	url        string
	user, pass string
	http       *http.Client
	nextID     atomic.Uint64
}

// rpcIdleTimeout is how long an idle connection to a node is kept.
const rpcIdleTimeout = 20 * time.Second

func newRPCClient(url, user, pass string) *rpcClient {
	return &rpcClient{
		url:  url,
		user: user,
		pass: pass,
		http: &http.Client{
			Timeout: 60 * time.Second,
			// Dogecoin Core closes a connection idle for 30 seconds
			// (-rpcservertimeout). Dropping ours sooner means a request
			// never goes out on one the server is closing, which fails
			// with EOF.
			Transport: &http.Transport{IdleConnTimeout: rpcIdleTimeout},
		},
	}
}

// rpcError is an error returned by the server.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message) }

// errNoAddressInfo is btcd's code for searchrawtransactions on an address
// with no transactions.
const errNoAddressInfo = -5

// call invokes method and decodes the result into result (unless nil).
func (c *rpcClient) call(result any, method string, params ...any) error {
	if params == nil {
		params = []any{}
	}
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "1.0",
		"id":      c.nextID.Add(1),
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.user != "" || c.pass != "" {
		req.SetBasicAuth(c.user, c.pass)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}

	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *rpcError       `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("%s: HTTP %d: %s", method, resp.StatusCode, bytes.TrimSpace(raw))
	}
	if envelope.Error != nil {
		return fmt.Errorf("%s: %w", method, envelope.Error)
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(envelope.Result, result)
}

func isRPCCode(err error, code int) bool {
	var rerr *rpcError
	return errors.As(err, &rerr) && rerr.Code == code
}
