package server

import "github.com/prometheus/client_golang/prometheus"

// Metrics are the token API's metrics.
type Metrics struct {
	requests       *prometheus.CounterVec
	githubDuration *prometheus.HistogramVec
	tokenCache     *prometheus.CounterVec
	tokenReviews   *prometheus.CounterVec
}

// NewMetrics returns the metrics, ready to be registered with Collectors.
func NewMetrics() *Metrics {
	return &Metrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "githubgate_requests_total",
			Help: "Requests answered, by mode, decision and HTTP status class.",
		}, []string{"mode", "decision", "code_class"}),
		githubDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "githubgate_github_request_duration_seconds",
			Help:    "Time taken by calls to GitHub, by operation.",
			Buckets: prometheus.DefBuckets,
		}, []string{"operation"}),
		tokenCache: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "githubgate_token_cache_total",
			Help: "Token cache lookups, by result (hit or miss).",
		}, []string{"result"}),
		tokenReviews: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "githubgate_tokenreview_total",
			Help: "Caller authentications, by result.",
		}, []string{"result"}),
	}
}

// Collectors returns the collectors to register.
func (m *Metrics) Collectors() []prometheus.Collector {
	return []prometheus.Collector{m.requests, m.githubDuration, m.tokenCache, m.tokenReviews}
}

func codeClass(status int) string {
	switch {
	case status >= 500:
		return "5xx"
	case status >= 400:
		return "4xx"
	case status >= 300:
		return "3xx"
	default:
		return "2xx"
	}
}
