// Package metrics is a dependency-free Prometheus exposition endpoint.
// It covers the RED basics (rate, errors, duration by route) plus a handful
// of business counters, with two rules that keep it safe:
//
//   - Route labels use the matched mux pattern ("POST /v1/orders"), never raw
//     paths: IDs in labels would create unbounded cardinality and melt any
//     Prometheus. Unmatched requests share one "unmatched" series.
//   - Children are created lazily under a mutex from fixed label sets, so the
//     series count stays small and predictable.
package metrics

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DurationBuckets are histogram edges in seconds, sized for an API whose
// p99 should sit far below the 10s write timeout.
var DurationBuckets = []float64{
	0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5,
}

type counter struct {
	labels map[string]int64
}

type histogram struct {
	buckets []float64
	counts  map[string][]int64
	sums    map[string]float64
}

// Registry holds every series. Construct one per process and share it;
// NewRegistry keeps tests isolated from each other.
type Registry struct {
	mu        sync.Mutex
	httpTotal map[string]int64
	httpHist  map[string][]int64
	httpSum   map[string]float64
	httpCount map[string]int64
	business  map[string]int64
}

func NewRegistry() *Registry {
	return &Registry{
		httpTotal: make(map[string]int64),
		httpHist:  make(map[string][]int64),
		httpSum:   make(map[string]float64),
		httpCount: make(map[string]int64),
		business:  make(map[string]int64),
	}
}

func httpKey(method, route string, status int) string {
	return method + "\x00" + route + "\x00" + strconv.Itoa(status)
}

func splitHTTPKey(key string) (method, route, status string) {
	parts := strings.Split(key, "\x00")

	return parts[0], parts[1], parts[2]
}

// ObserveHTTP records one finished request. Call it from middleware, after
// the handler ran, so status is final.
func (r *Registry) ObserveHTTP(method, route string, status int, d time.Duration) {
	key := httpKey(method, route, status)
	secs := d.Seconds()

	r.mu.Lock()
	defer r.mu.Unlock()

	r.httpTotal[key]++

	counts, ok := r.httpHist[key]
	if !ok {
		counts = make([]int64, len(DurationBuckets)+1)
		r.httpHist[key] = counts
	}

	placed := false

	for i, edge := range DurationBuckets {
		if secs <= edge {
			counts[i]++
			placed = true

			break
		}
	}

	if !placed {
		counts[len(DurationBuckets)]++
	}

	r.httpSum[key] += secs
	r.httpCount[key]++
}

// Business counters: incremented on first completion only (created=true),
// so retries never inflate them. Label values come from fixed sets.
func (r *Registry) IncOrdersCreated() {
	r.addBusiness("minipay_orders_created_total", "")
}

func (r *Registry) IncPaymentsCreated(status string) {
	r.addBusiness("minipay_payments_created_total", status)
}

func (r *Registry) IncRefundsCreated() {
	r.addBusiness("minipay_refunds_created_total", "")
}

func (r *Registry) IncDisputesOpened() {
	r.addBusiness("minipay_disputes_opened_total", "")
}

func (r *Registry) AddWebhookSettled(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.business["minipay_webhook_deliveries_settled_total\x00"] += int64(n)
}

func (r *Registry) addBusiness(name, label string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.business[name+"\x00"+label]++
}

func escapeLabelValue(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)

	return s
}

// Handler serves the Prometheus text exposition format (0.0.4): HELP/TYPE
// comments plus one line per series. Point Prometheus at /metrics.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		r.mu.Lock()
		defer r.mu.Unlock()

		var b strings.Builder

		fmt.Fprintf(&b, "# HELP minipay_http_requests_total Finished HTTP requests.\n")
		fmt.Fprintf(&b, "# TYPE minipay_http_requests_total counter\n")

		for _, key := range sortedKeys(r.httpTotal) {
			method, route, status := splitHTTPKey(key)
			fmt.Fprintf(&b, "minipay_http_requests_total{method=%q,route=%q,status=%q} %d\n",
				escapeLabelValue(method), escapeLabelValue(route), status, r.httpTotal[key])
		}

		fmt.Fprintf(&b, "# HELP minipay_http_request_duration_seconds Request latency.\n")
		fmt.Fprintf(&b, "# TYPE minipay_http_request_duration_seconds histogram\n")

		for _, key := range sortedHistKeys(r.httpHist) {
			method, route, status := splitHTTPKey(key)
			labels := fmt.Sprintf("method=%q,route=%q,status=%q",
				escapeLabelValue(method), escapeLabelValue(route), status)
			counts := r.httpHist[key]
			cumulative := int64(0)

			for i, edge := range DurationBuckets {
				cumulative += counts[i]
				fmt.Fprintf(&b, "minipay_http_request_duration_seconds_bucket{%s,le=%g} %d\n",
					labels, edge, cumulative)
			}

			cumulative += counts[len(DurationBuckets)]
			fmt.Fprintf(&b, "minipay_http_request_duration_seconds_bucket{%s,le=\"+Inf\"} %d\n",
				labels, cumulative)
			fmt.Fprintf(&b, "minipay_http_request_duration_seconds_sum{%s} %g\n",
				labels, r.httpSum[key])
			fmt.Fprintf(&b, "minipay_http_request_duration_seconds_count{%s} %d\n",
				labels, r.httpCount[key])
		}

		fmt.Fprintf(&b, "# HELP minipay_business_total First-completion business events.\n")
		fmt.Fprintf(&b, "# TYPE minipay_business_total counter\n")

		for _, key := range sortedKeys(r.business) {
			name, label, _ := strings.Cut(key, "\x00")

			if label == "" {
				fmt.Fprintf(&b, "%s %d\n", name, r.business[key])
			} else {
				fmt.Fprintf(&b, "%s{status=%q} %d\n", name, escapeLabelValue(label), r.business[key])
			}
		}

		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte(b.String()))
	})
}

func sortedKeys(m map[string]int64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	return keys
}

func sortedHistKeys(m map[string][]int64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	return keys
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (rec *statusRecorder) WriteHeader(code int) {
	rec.status = code
	rec.ResponseWriter.WriteHeader(code)
}

// Middleware records rate, errors, and latency per matched route pattern.
// Apply it around the API mux (inside auth): r.Pattern is only populated
// for the mux that matched, which is exactly the low-cardinality label we
// want. Unmatched requests share the "unmatched" series.
func (r *Registry) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, rr *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rec, rr)

		route := rr.Pattern
		if route == "" {
			route = "unmatched"
		}

		r.ObserveHTTP(rr.Method, route, rec.status, time.Since(start))
	})
}
