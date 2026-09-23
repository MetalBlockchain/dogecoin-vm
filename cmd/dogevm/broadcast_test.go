package main

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// A node's refusal means the transaction was not sent; no answer means it
// may have been, and the wallet must not forget it.
func TestBroadcastError(t *testing.T) {
	var aerr *apiError
	refused := fmt.Errorf("sendrawtransaction: %w", &rpcError{Code: -26, Message: "dust"})
	require.True(t, errors.As(broadcastError(refused), &aerr))
	require.Equal(t, http.StatusBadRequest, aerr.status)
	require.Contains(t, aerr.msg, "dust")

	lost := fmt.Errorf("sendrawtransaction: %w", errors.New("connection reset by peer"))
	require.True(t, errors.As(broadcastError(lost), &aerr))
	require.Equal(t, http.StatusBadGateway, aerr.status)
	require.Contains(t, aerr.msg, "may or may not have been sent")
}
