package main

import "net/http"

// This relay does not meter messages. The hosted CLI (@marshell/cli) still expects a wallet on every
// send and a GET /v1/wallet route, and treats a missing one as an error, so report a balance that
// never runs out. A billing relay would replace unmeteredWallet with a real ledger.
const unmeteredMessages = 1_000_000_000

func unmeteredWallet() map[string]int {
	return map[string]int{
		"free_remaining":     unmeteredMessages,
		"prepaid_balance":    0,
		"messages_remaining": unmeteredMessages,
	}
}

func walletHandler(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"wallet": unmeteredWallet()})
}
