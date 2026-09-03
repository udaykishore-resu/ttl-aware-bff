package observability

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/udaykishore/ttl-aware-bff/internal/testutil"
)

// meterFixture builds a meter over a ManualReader, with fixed histogram
// boundaries so the exposition is byte-predictable rather than depending on
// whatever default the SDK ships this release.
func meterFixture() (*sdkmetric.ManualReader, metric.Meter) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithView(sdkmetric.NewView(
			sdkmetric.Instrument{Kind: sdkmetric.InstrumentKindHistogram},
			sdkmetric.Stream{Aggregation: sdkmetric.AggregationExplicitBucketHistogram{
				Boundaries: []float64{1, 2, 5},
			}},
		)),
	)
	return reader, mp.Meter(InstrumentationName)
}

// collect renders the reader's current state.
func collect(t *testing.T, reader *sdkmetric.ManualReader) string {
	t.Helper()
	text, err := CollectText(context.Background(), reader)
	testutil.NoError(t, err, "collect")
	return text
}

// lineWith returns the single emitted line containing substr.
func lineWith(t *testing.T, text, substr string) string {
	t.Helper()
	var found []string
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, substr) {
			found = append(found, line)
		}
	}
	if len(found) != 1 {
		t.Fatalf("expected exactly one line containing %q, got %d:\n%s", substr, len(found), text)
	}
	return found[0]
}

// TestCollectText_CounterFamily verifies REQ-OBS-011: the in-process endpoint
// emits well-formed Prometheus text -- a HELP line, a TYPE line and one sample
// per attribute set. A scraper rejects the whole payload if any of that is
// missing, so an endpoint that is almost right is an endpoint that is down.
func TestCollectText_CounterFamily(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reader, meter := meterFixture()

	c, err := meter.Int64Counter("bff_request_total",
		metric.WithDescription("API requests served, by route and outcome"),
		metric.WithUnit("{request}"))
	testutil.NoError(t, err, "instrument")

	c.Add(ctx, 3, metric.WithAttributes(attribute.String("route", "/status")))
	c.Add(ctx, 1, metric.WithAttributes(attribute.String("route", "/details")))

	text := collect(t, reader)

	testutil.True(t, strings.Contains(text,
		"# HELP bff_request_total API requests served, by route and outcome\n"),
		"the description is emitted as HELP, got:\n%s", text)
	testutil.True(t, strings.Contains(text, "# TYPE bff_request_total counter\n"),
		"a monotonic sum is a counter, got:\n%s", text)
	testutil.Equal(t, lineWith(t, text, `route="/status"`), `bff_request_total{route="/status"} 3`, "sample")
	testutil.Equal(t, lineWith(t, text, `route="/details"`), `bff_request_total{route="/details"} 1`, "sample")

	t.Run("HELP and TYPE precede the samples", func(t *testing.T) {
		t.Parallel()
		help := strings.Index(text, "# HELP bff_request_total")
		typ := strings.Index(text, "# TYPE bff_request_total")
		sample := strings.Index(text, `bff_request_total{route=`)
		testutil.True(t, help < typ && typ < sample,
			"the family header must come before its samples, got:\n%s", text)
	})

	t.Run("an instrument with no description emits no HELP line", func(t *testing.T) {
		t.Parallel()
		reader, meter := meterFixture()
		u, err := meter.Int64Counter("bff_undocumented_total")
		testutil.NoError(t, err, "instrument")
		u.Add(ctx, 1)
		text := collect(t, reader)
		testutil.False(t, strings.Contains(text, "# HELP bff_undocumented_total"),
			"an empty description must not produce an empty HELP line, got:\n%s", text)
		testutil.True(t, strings.Contains(text, "# TYPE bff_undocumented_total counter"), "TYPE is still emitted")
		testutil.Equal(t, lineWith(t, text, "bff_undocumented_total 1"), "bff_undocumented_total 1",
			"a sample with no attributes carries no label braces")
	})
}

// TestCollectText_NonMonotonicSumsAndGauges verifies REQ-OBS-011: an up-down
// counter is not a Prometheus counter. Declaring one as such would make every
// rate() over it produce nonsense the first time it decreases.
func TestCollectText_NonMonotonicSumsAndGauges(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reader, meter := meterFixture()

	inflight, err := meter.Int64UpDownCounter("bff_concurrent_requests",
		metric.WithDescription("API requests currently in flight"))
	testutil.NoError(t, err, "up-down counter")
	inflight.Add(ctx, 5)
	inflight.Add(ctx, -2)

	state, err := meter.Int64Gauge("breaker_state", metric.WithDescription("Circuit breaker state"))
	testutil.NoError(t, err, "gauge")
	state.Record(ctx, 2, metric.WithAttributes(attribute.String("source", "execution")))

	text := collect(t, reader)

	testutil.True(t, strings.Contains(text, "# TYPE bff_concurrent_requests gauge\n"),
		"a non-monotonic sum is exposed as a gauge, got:\n%s", text)
	testutil.Equal(t, lineWith(t, text, "bff_concurrent_requests 3"), "bff_concurrent_requests 3",
		"the value is the running total")
	testutil.True(t, strings.Contains(text, "# TYPE breaker_state gauge\n"), "gauge type")
	testutil.Equal(t, lineWith(t, text, `breaker_state{source=`), `breaker_state{source="execution"} 2`, "gauge sample")
}

// TestCollectText_HistogramFamily verifies REQ-OBS-011: a histogram is exposed
// as the three families a scraper expects -- _bucket, _sum and _count -- with
// cumulative bucket counts and a closing +Inf bucket. Prometheus computes
// quantiles from the cumulative sequence, so emitting per-bucket counts instead
// would produce silently wrong latency numbers rather than an obvious failure.
func TestCollectText_HistogramFamily(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reader, meter := meterFixture()

	h, err := meter.Float64Histogram("bff_request_latency",
		metric.WithDescription("End-to-end API request latency"), metric.WithUnit("s"))
	testutil.NoError(t, err, "instrument")

	// Boundaries are 1, 2 and 5, so these land in the first, second and last
	// buckets respectively.
	for _, v := range []float64{0.5, 1.5, 1.5, 7} {
		h.Record(ctx, v)
	}

	text := collect(t, reader)
	testutil.True(t, strings.Contains(text, "# TYPE bff_request_latency histogram\n"),
		"histogram type, got:\n%s", text)

	bucketRe := regexp.MustCompile(`^bff_request_latency_bucket\{le="([^"]+)"\} (\S+)$`)
	var bounds []string
	var counts []float64
	for _, line := range strings.Split(text, "\n") {
		if m := bucketRe.FindStringSubmatch(line); m != nil {
			v, err := strconv.ParseFloat(m[2], 64)
			testutil.NoError(t, err, "bucket count %q is a number", m[2])
			bounds = append(bounds, m[1])
			counts = append(counts, v)
		}
	}

	testutil.Equal(t, bounds, []string{"1", "2", "5", "+Inf"},
		"every configured boundary is emitted, closed by +Inf")
	testutil.Equal(t, counts, []float64{1, 3, 3, 4}, "bucket counts are cumulative")
	for i := 1; i < len(counts); i++ {
		testutil.True(t, counts[i] >= counts[i-1],
			"a cumulative sequence never decreases: %v", counts)
	}

	testutil.Equal(t, lineWith(t, text, "bff_request_latency_sum"), "bff_request_latency_sum 10.5", "sum")
	testutil.Equal(t, lineWith(t, text, "bff_request_latency_count"), "bff_request_latency_count 4", "count")
	testutil.Equal(t, counts[len(counts)-1], float64(4),
		"the +Inf bucket must equal the observation count, or the scraper rejects the family")

	t.Run("labels survive onto every bucket", func(t *testing.T) {
		t.Parallel()
		reader, meter := meterFixture()
		h, err := meter.Float64Histogram("source_latency", metric.WithDescription("Source latency"))
		testutil.NoError(t, err, "instrument")
		h.Record(ctx, 0.5, metric.WithAttributes(attribute.String("source", "operational")))

		text := collect(t, reader)
		testutil.True(t, strings.Contains(text, `source_latency_bucket{le="1",source="operational"} 1`),
			"the data point's attributes are rendered alongside le, sorted, got:\n%s", text)
		testutil.True(t, strings.Contains(text, `source_latency_count{source="operational"} 1`),
			"and on the count family too, got:\n%s", text)
	})
}

// TestCollectText_LabelsAreSortedAndSanitised verifies REQ-OBS-011: labels are
// emitted in a stable order with names that are legal Prometheus identifiers.
// OpenTelemetry attribute keys are dotted by convention and Prometheus does not
// accept dots, so the translation has to happen here rather than at the scraper.
func TestCollectText_LabelsAreSortedAndSanitised(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reader, meter := meterFixture()

	c, err := meter.Int64Counter("routing_decision_total", metric.WithDescription("Routing decisions"))
	testutil.NoError(t, err, "instrument")
	c.Add(ctx, 1, metric.WithAttributes(
		attribute.String("zeta", "z"),
		attribute.String("http.route", "/status"),
		attribute.String("alpha", "a"),
	))

	text := collect(t, reader)
	line := lineWith(t, text, "routing_decision_total{")
	testutil.Equal(t, line, `routing_decision_total{alpha="a",http_route="/status",zeta="z"} 1`,
		"labels are sorted by name and dots become underscores")

	t.Run("the metric name is sanitised too", func(t *testing.T) {
		t.Parallel()
		reader, meter := meterFixture()
		c, err := meter.Int64Counter("bff.cache.hit.total", metric.WithDescription("Cache hits"))
		testutil.NoError(t, err, "instrument")
		c.Add(ctx, 1)
		text := collect(t, reader)
		testutil.True(t, strings.Contains(text, "# TYPE bff_cache_hit_total counter"),
			"a dotted instrument name becomes a legal metric name, got:\n%s", text)
	})
}

// TestCollectText_HostileLabelValuesAreEscaped verifies REQ-OBS-011 against the
// one input that is not under this service's control: a label value carrying a
// quote, a backslash or a newline. Emitting any of them raw would terminate the
// label set or the line early, and the scraper would reject the entire payload
// -- so one odd tenant id would take out every metric the service exports.
func TestCollectText_HostileLabelValuesAreEscaped(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reader, meter := meterFixture()

	c, err := meter.Int64Counter("datasource_error_total", metric.WithDescription("Source errors"))
	testutil.NoError(t, err, "instrument")

	hostile := "quote:\" backslash:\\ newline:\n end"
	c.Add(ctx, 1, metric.WithAttributes(attribute.String("detail", hostile)))

	text := collect(t, reader)
	line := lineWith(t, text, "datasource_error_total{")

	testutil.Equal(t, line, `datasource_error_total{detail="quote:\" backslash:\\ newline:\n end"} 1`,
		"each hostile character is escaped rather than emitted raw")
	testutil.False(t, strings.Contains(line, "\n"), "the value's newline did not break the line in two")

	t.Run("the payload still parses as one sample per line", func(t *testing.T) {
		t.Parallel()
		// Every non-comment line must be `name{labels} value`, with the value
		// as the last space-separated token.
		for _, l := range strings.Split(strings.TrimSpace(text), "\n") {
			if strings.HasPrefix(l, "#") {
				continue
			}
			idx := strings.LastIndex(l, " ")
			testutil.True(t, idx > 0, "line %q has a value", l)
			_, err := strconv.ParseFloat(l[idx+1:], 64)
			testutil.NoError(t, err, "the trailing token of %q is a number", l)
		}
	})

	t.Run("HELP text is escaped as well", func(t *testing.T) {
		t.Parallel()
		reader, meter := meterFixture()
		c, err := meter.Int64Counter("odd_help_total",
			metric.WithDescription("first line\nsecond line with a \\ backslash"))
		testutil.NoError(t, err, "instrument")
		c.Add(ctx, 1)
		text := collect(t, reader)
		testutil.True(t, strings.Contains(text,
			`# HELP odd_help_total first line\nsecond line with a \\ backslash`+"\n"),
			"a multi-line description must not become two HELP lines, got:\n%s", text)
	})
}

// TestPrometheusHandler verifies REQ-OBS-011: the exposition is reachable over
// HTTP with the content type a scraper expects, and the endpoint reports itself
// absent -- rather than empty or broken -- when it is not configured.
func TestPrometheusHandler(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reader, meter := meterFixture()
	c, err := meter.Int64Counter("bff_request_total", metric.WithDescription("API requests served"))
	testutil.NoError(t, err, "instrument")
	c.Add(ctx, 2, metric.WithAttributes(attribute.String("route", "/status")))

	rec := httptest.NewRecorder()
	PrometheusHandler(reader).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	testutil.Equal(t, rec.Code, http.StatusOK, "status")
	testutil.Equal(t, rec.Header().Get("Content-Type"), "text/plain; version=0.0.4; charset=utf-8",
		"the exposition format is declared in the content type")
	testutil.True(t, strings.Contains(rec.Body.String(), `bff_request_total{route="/status"} 2`),
		"the body carries the samples, got:\n%s", rec.Body.String())

	t.Run("a disabled endpoint is a 404", func(t *testing.T) {
		t.Parallel()
		rec := httptest.NewRecorder()
		PrometheusHandler(nil).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		testutil.Equal(t, rec.Code, http.StatusNotFound,
			"with no reader configured the endpoint does not exist, rather than serving nothing")
	})
}

// TestCollectText_ServiceInstrumentsRoundTrip verifies REQ-OBS-011 against the
// instruments the service actually declares: every one of them has to survive
// the encoder, because a name or unit that the encoder mangles would be
// discovered in production rather than here.
func TestCollectText_ServiceInstrumentsRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reader, meter := meterFixture()

	m, err := NewMetrics(meter)
	testutil.NoError(t, err, "the service's instrument set is constructible")

	m.RequestTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("request_type", "resource_status")))
	m.RequestLatency.Record(ctx, 0.42, metric.WithAttributes(attribute.String("request_type", "resource_status")))
	m.TTLHitTotal.Add(ctx, 1)
	m.CacheHitTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("layer", "L1")))
	m.PrecedenceConflicts.Add(ctx, 1, metric.WithAttributes(attribute.String("field", "status")))

	text := collect(t, reader)

	for _, want := range []string{
		"# TYPE bff_request_total counter",
		"# TYPE bff_request_latency histogram",
		"# TYPE operational_ttl_hit_total counter",
		"# TYPE cache_hit_total counter",
		`cache_hit_total{layer="L1"} 1`,
		`precedence_conflict_total{field="status"} 1`,
		"bff_request_latency_count",
	} {
		testutil.True(t, strings.Contains(text, want), "exposition must contain %q, got:\n%s", want, text)
	}
}
