package main

import (
	"fmt"
	"net/http"
	"sync/atomic"
)

var (
	metricSendsTotal       atomic.Uint64
	metricAcksTotal        atomic.Uint64
	metricRateLimitedTotal atomic.Uint64
	metricSendDedupedTotal atomic.Uint64
)

func metricsIncSends()       { metricSendsTotal.Add(1) }
func metricsIncAcks()        { metricAcksTotal.Add(1) }
func metricsIncRateLimited() { metricRateLimitedTotal.Add(1) }
func metricsIncSendDeduped() { metricSendDedupedTotal.Add(1) }

func metricsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		fmt.Fprintf(w, "# HELP marshell_sends_total Outbound messages accepted by the relay.\n")
		fmt.Fprintf(w, "# TYPE marshell_sends_total counter\n")
		fmt.Fprintf(w, "marshell_sends_total %d\n", metricSendsTotal.Load())
		fmt.Fprintf(w, "# HELP marshell_send_deduped_total Duplicate sends collapsed within dedup window.\n")
		fmt.Fprintf(w, "# TYPE marshell_send_deduped_total counter\n")
		fmt.Fprintf(w, "marshell_send_deduped_total %d\n", metricSendDedupedTotal.Load())
		fmt.Fprintf(w, "# HELP marshell_acks_total Inbox messages acknowledged by recipients.\n")
		fmt.Fprintf(w, "# TYPE marshell_acks_total counter\n")
		fmt.Fprintf(w, "marshell_acks_total %d\n", metricAcksTotal.Load())
		fmt.Fprintf(w, "# HELP marshell_rate_limited_total Requests rejected with HTTP 429.\n")
		fmt.Fprintf(w, "# TYPE marshell_rate_limited_total counter\n")
		fmt.Fprintf(w, "marshell_rate_limited_total %d\n", metricRateLimitedTotal.Load())
	}
}
