package server

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds all Prometheus metrics for the relay server.
type Metrics struct {
	Registry         *prometheus.Registry
	ActiveTunnels    *prometheus.GaugeVec
	HTTPRequests     *prometheus.CounterVec
	HTTPDuration     *prometheus.HistogramVec
	StreamsTotal     prometheus.Counter
	StreamsActive    prometheus.Gauge
	BytesTransferred *prometheus.CounterVec
}

// NewMetrics creates and registers all Prometheus metrics with a dedicated registry.
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	// Include default Go and process metrics.
	reg.MustRegister(prometheus.NewGoCollector())
	reg.MustRegister(prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))

	m := &Metrics{Registry: reg}

	m.ActiveTunnels = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "tunnel_active_connections",
		Help: "Number of active tunnel connections",
	}, []string{"mode"})
	reg.MustRegister(m.ActiveTunnels)

	m.HTTPRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "tunnel_http_requests_total",
		Help: "Total HTTP requests proxied",
	}, []string{"method", "status_code"})
	reg.MustRegister(m.HTTPRequests)

	m.HTTPDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "tunnel_http_request_duration_seconds",
		Help:    "HTTP request latency in seconds",
		Buckets: prometheus.DefBuckets,
	}, []string{"method"})
	reg.MustRegister(m.HTTPDuration)

	m.StreamsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "tunnel_streams_total",
		Help: "Total TCP/UDP streams opened",
	})
	reg.MustRegister(m.StreamsTotal)

	m.StreamsActive = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "tunnel_streams_active",
		Help: "Currently active TCP/UDP streams",
	})
	reg.MustRegister(m.StreamsActive)

	m.BytesTransferred = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "tunnel_bytes_transferred_total",
		Help: "Total bytes transferred through tunnels",
	}, []string{"direction"})
	reg.MustRegister(m.BytesTransferred)

	return m
}

// Handler returns an HTTP handler for the /metrics endpoint.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{})
}

// OnTunnelConnect records a new tunnel connection.
func (m *Metrics) OnTunnelConnect(mode string) {
	m.ActiveTunnels.WithLabelValues(mode).Inc()
}

// OnTunnelDisconnect records a tunnel disconnection.
func (m *Metrics) OnTunnelDisconnect(mode string) {
	m.ActiveTunnels.WithLabelValues(mode).Dec()
}

// OnHTTPRequest records an HTTP request.
func (m *Metrics) OnHTTPRequest(method, statusCode string, durationSeconds float64) {
	m.HTTPRequests.WithLabelValues(method, statusCode).Inc()
	m.HTTPDuration.WithLabelValues(method).Observe(durationSeconds)
}

// OnStreamOpen records a new stream.
func (m *Metrics) OnStreamOpen() {
	m.StreamsTotal.Inc()
	m.StreamsActive.Inc()
}

// OnStreamClose records a stream closure.
func (m *Metrics) OnStreamClose() {
	m.StreamsActive.Dec()
}

// OnBytes records bytes transferred.
func (m *Metrics) OnBytes(direction string, n int) {
	m.BytesTransferred.WithLabelValues(direction).Add(float64(n))
}
