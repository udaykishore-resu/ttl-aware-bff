package observability

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Metrics holds every instrument the service emits. Constructing them once and
// passing the struct around means an instrument name can never be typed twice
// with two spellings, and it keeps the hot path free of map lookups.
//
// Cardinality discipline: tenant_id is included only on request-level
// instruments. Per-source and per-rule instruments deliberately omit it,
// because tenant x source x rule is how a metrics bill gets out of hand
// (REQ-OBS-013).
type Metrics struct {
	RequestTotal       metric.Int64Counter
	RequestLatency     metric.Float64Histogram
	ConcurrentReqs     metric.Int64UpDownCounter
	OperationalLatency metric.Float64Histogram
	ExecutionLatency   metric.Float64Histogram
	AggregationLatency metric.Float64Histogram
	DataFreshnessAge   metric.Float64Histogram

	TTLHitTotal      metric.Int64Counter
	TTLMissTotal     metric.Int64Counter
	FallbackTotal    metric.Int64Counter
	RoutingDecision  metric.Int64Counter
	DataSourceErrors metric.Int64Counter

	CacheHitTotal  metric.Int64Counter
	CacheMissTotal metric.Int64Counter
	CacheErrors    metric.Int64Counter

	PartialResponses metric.Int64Counter
	StaleResponses   metric.Int64Counter

	BreakerTransitions metric.Int64Counter
	BreakerState       metric.Int64Gauge
	BulkheadInFlight   metric.Int64UpDownCounter
	BulkheadRejections metric.Int64Counter
	RateLimited        metric.Int64Counter

	PrecedenceConflicts metric.Int64Counter
	SchemaMismatch      metric.Int64Counter
	ClockSkewDetected   metric.Int64Counter
	ConfigReloads       metric.Int64Counter
}

// NewMetrics creates every instrument against the supplied meter.
func NewMetrics(m metric.Meter) (*Metrics, error) {
	var err error
	out := &Metrics{}

	counter := func(name, desc, unit string) metric.Int64Counter {
		if err != nil {
			return nil
		}
		var c metric.Int64Counter
		c, err = m.Int64Counter(name, metric.WithDescription(desc), metric.WithUnit(unit))
		if err != nil {
			err = fmt.Errorf("observability: instrument %s: %w", name, err)
		}
		return c
	}
	hist := func(name, desc, unit string) metric.Float64Histogram {
		if err != nil {
			return nil
		}
		var h metric.Float64Histogram
		h, err = m.Float64Histogram(name, metric.WithDescription(desc), metric.WithUnit(unit))
		if err != nil {
			err = fmt.Errorf("observability: instrument %s: %w", name, err)
		}
		return h
	}
	updown := func(name, desc, unit string) metric.Int64UpDownCounter {
		if err != nil {
			return nil
		}
		var c metric.Int64UpDownCounter
		c, err = m.Int64UpDownCounter(name, metric.WithDescription(desc), metric.WithUnit(unit))
		if err != nil {
			err = fmt.Errorf("observability: instrument %s: %w", name, err)
		}
		return c
	}
	gauge := func(name, desc, unit string) metric.Int64Gauge {
		if err != nil {
			return nil
		}
		var g metric.Int64Gauge
		g, err = m.Int64Gauge(name, metric.WithDescription(desc), metric.WithUnit(unit))
		if err != nil {
			err = fmt.Errorf("observability: instrument %s: %w", name, err)
		}
		return g
	}

	out.RequestTotal = counter("bff_request_total", "API requests served, by route and outcome", "{request}")
	out.RequestLatency = hist("bff_request_latency", "End-to-end API request latency", "s")
	out.ConcurrentReqs = updown("bff_concurrent_requests", "API requests currently in flight", "{request}")
	out.OperationalLatency = hist("operational_source_latency", "Latency of calls to the Operational Data Source", "s")
	out.ExecutionLatency = hist("execution_source_latency", "Latency of calls to the Execution Data Source", "s")
	out.AggregationLatency = hist("aggregation_latency", "Time spent fanning out and merging source results", "s")
	out.DataFreshnessAge = hist("data_freshness_age", "Age of the data served, as evaluated against its TTL", "s")

	out.TTLHitTotal = counter("operational_ttl_hit_total", "Requests where operational data was within TTL", "{request}")
	out.TTLMissTotal = counter("operational_ttl_miss_total", "Requests where operational data exceeded TTL", "{request}")
	out.FallbackTotal = counter("execution_fallback_total", "Requests redirected to the Execution source after an operational miss or failure", "{request}")
	out.RoutingDecision = counter("routing_decision_total", "Routing decisions, by target and rule", "{decision}")
	out.DataSourceErrors = counter("datasource_error_total", "Errors returned by a data source, by source and error code", "{error}")

	out.CacheHitTotal = counter("cache_hit_total", "Cache hits, by layer", "{lookup}")
	out.CacheMissTotal = counter("cache_miss_total", "Cache misses", "{lookup}")
	out.CacheErrors = counter("cache_error_total", "Cache backend errors (fail-open)", "{error}")

	out.PartialResponses = counter("partial_response_total", "Responses served with one or more sources missing", "{response}")
	out.StaleResponses = counter("stale_response_total", "Responses served from data past its freshness TTL", "{response}")

	out.BreakerTransitions = counter("circuit_breaker_transition_total", "Circuit breaker state transitions", "{transition}")
	out.BreakerState = gauge("circuit_breaker_state", "Circuit breaker state: 0 closed, 1 half-open, 2 open", "{state}")
	out.BulkheadInFlight = updown("bulkhead_in_flight", "Calls currently held by a source bulkhead", "{call}")
	out.BulkheadRejections = counter("bulkhead_rejected_total", "Calls rejected because a bulkhead was saturated", "{call}")
	out.RateLimited = counter("rate_limited_total", "Requests rejected by the rate limiter", "{request}")

	out.PrecedenceConflicts = counter("precedence_conflict_total", "Fields where two sources disagreed and precedence had to choose", "{field}")
	out.SchemaMismatch = counter("schema_version_mismatch_total", "Source responses declaring an unsupported schema version", "{response}")
	out.ClockSkewDetected = counter("clock_skew_detected_total", "Freshness evaluations where source and BFF clocks disagreed beyond tolerance", "{evaluation}")
	out.ConfigReloads = counter("config_reload_total", "Configuration reloads, by outcome", "{reload}")

	if err != nil {
		return nil, err
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Recording helpers. These exist so call sites cannot forget an attribute.
// ---------------------------------------------------------------------------

// RecordSourceLatency records a source call's duration on the right histogram.
func (m *Metrics) RecordSourceLatency(ctx context.Context, source string, d time.Duration, attrs ...attribute.KeyValue) {
	if m == nil {
		return
	}
	set := metric.WithAttributes(attrs...)
	switch source {
	case "OPERATIONAL":
		m.OperationalLatency.Record(ctx, d.Seconds(), set)
	case "EXECUTION":
		m.ExecutionLatency.Record(ctx, d.Seconds(), set)
	}
}

// RecordTTL records the outcome of a freshness evaluation.
func (m *Metrics) RecordTTL(ctx context.Context, hit bool, attrs ...attribute.KeyValue) {
	if m == nil {
		return
	}
	set := metric.WithAttributes(attrs...)
	if hit {
		m.TTLHitTotal.Add(ctx, 1, set)
		return
	}
	m.TTLMissTotal.Add(ctx, 1, set)
}

// RecordCache records a cache lookup outcome.
func (m *Metrics) RecordCache(ctx context.Context, hit bool, layer string) {
	if m == nil {
		return
	}
	set := metric.WithAttributes(attribute.String(AttrCacheLayer, layer))
	if hit {
		m.CacheHitTotal.Add(ctx, 1, set)
		return
	}
	m.CacheMissTotal.Add(ctx, 1, set)
}

// BreakerStateValue maps a breaker state name to its numeric gauge value.
func BreakerStateValue(state string) int64 {
	switch state {
	case "closed":
		return 0
	case "half-open":
		return 1
	case "open":
		return 2
	default:
		return -1
	}
}
