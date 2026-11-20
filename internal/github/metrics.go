package github

import "github.com/prometheus/client_golang/prometheus"

// Metrics are the GitHub client's metrics.
type Metrics struct {
	rateLimitRemaining *prometheus.GaugeVec
}

// NewMetrics returns the metrics, ready to be registered with Collectors.
func NewMetrics() *Metrics {
	return &Metrics{
		rateLimitRemaining: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "githubgate_github_rate_limit_remaining",
			Help: "Requests left in the GitHub rate limit of an installation, read after a token is minted.",
		}, []string{"installation"}),
	}
}

// Collectors returns the collectors to register.
func (m *Metrics) Collectors() []prometheus.Collector {
	return []prometheus.Collector{m.rateLimitRemaining}
}
