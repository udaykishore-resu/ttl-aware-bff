package observability

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// PrometheusHandler exposes the metrics collected by a ManualReader in the
// Prometheus text exposition format.
//
// Why hand-rolled rather than the OTel Prometheus exporter: the exporter pulls
// in the full Prometheus client library for what amounts to a text encoder,
// and the recommended production topology here is OTLP push to a collector
// that owns the scrape endpoint. This endpoint exists for the cases where a
// collector is not in the path -- a bare Kubernetes cluster with a
// ServiceMonitor, or a developer with curl -- so it is kept small and
// dependency-free on purpose (REQ-OBS-011).
func PrometheusHandler(reader *sdkmetric.ManualReader) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reader == nil {
			http.Error(w, "metrics endpoint disabled", http.StatusNotFound)
			return
		}
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(r.Context(), &rm); err != nil {
			http.Error(w, "metric collection failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		writeExposition(w, &rm)
	})
}

// CollectText renders the current metrics as Prometheus text. Used by tests.
func CollectText(ctx context.Context, reader *sdkmetric.ManualReader) (string, error) {
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		return "", err
	}
	var sb strings.Builder
	writeExposition(&sb, &rm)
	return sb.String(), nil
}

func writeExposition(w io.Writer, rm *metricdata.ResourceMetrics) {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			name := sanitizeMetricName(m.Name)
			if m.Description != "" {
				fmt.Fprintf(w, "# HELP %s %s\n", name, escapeHelp(m.Description))
			}
			switch data := m.Data.(type) {
			case metricdata.Sum[int64]:
				fmt.Fprintf(w, "# TYPE %s %s\n", name, sumType(data.IsMonotonic))
				for _, dp := range data.DataPoints {
					writeSample(w, name, "", dp.Attributes, float64(dp.Value))
				}
			case metricdata.Sum[float64]:
				fmt.Fprintf(w, "# TYPE %s %s\n", name, sumType(data.IsMonotonic))
				for _, dp := range data.DataPoints {
					writeSample(w, name, "", dp.Attributes, dp.Value)
				}
			case metricdata.Gauge[int64]:
				fmt.Fprintf(w, "# TYPE %s gauge\n", name)
				for _, dp := range data.DataPoints {
					writeSample(w, name, "", dp.Attributes, float64(dp.Value))
				}
			case metricdata.Gauge[float64]:
				fmt.Fprintf(w, "# TYPE %s gauge\n", name)
				for _, dp := range data.DataPoints {
					writeSample(w, name, "", dp.Attributes, dp.Value)
				}
			case metricdata.Histogram[float64]:
				fmt.Fprintf(w, "# TYPE %s histogram\n", name)
				for _, dp := range data.DataPoints {
					writeHistogram(w, name, dp)
				}
			case metricdata.Histogram[int64]:
				fmt.Fprintf(w, "# TYPE %s histogram\n", name)
				for _, dp := range data.DataPoints {
					writeIntHistogram(w, name, dp)
				}
			}
		}
	}
}

func sumType(monotonic bool) string {
	if monotonic {
		return "counter"
	}
	return "gauge"
}

func writeHistogram(w io.Writer, name string, dp metricdata.HistogramDataPoint[float64]) {
	var cumulative uint64
	for i, b := range dp.Bounds {
		cumulative += dp.BucketCounts[i]
		writeSampleWithExtra(w, name+"_bucket", dp.Attributes, "le", formatFloat(b), float64(cumulative))
	}
	writeSampleWithExtra(w, name+"_bucket", dp.Attributes, "le", "+Inf", float64(dp.Count))
	writeSample(w, name+"_sum", "", dp.Attributes, dp.Sum)
	writeSample(w, name+"_count", "", dp.Attributes, float64(dp.Count))
}

func writeIntHistogram(w io.Writer, name string, dp metricdata.HistogramDataPoint[int64]) {
	var cumulative uint64
	for i, b := range dp.Bounds {
		cumulative += dp.BucketCounts[i]
		writeSampleWithExtra(w, name+"_bucket", dp.Attributes, "le", formatFloat(b), float64(cumulative))
	}
	writeSampleWithExtra(w, name+"_bucket", dp.Attributes, "le", "+Inf", float64(dp.Count))
	writeSample(w, name+"_sum", "", dp.Attributes, float64(dp.Sum))
	writeSample(w, name+"_count", "", dp.Attributes, float64(dp.Count))
}

func writeSample(w io.Writer, name, _ string, attrs attributeSet, value float64) {
	writeSampleWithExtra(w, name, attrs, "", "", value)
}

// attributeSet is the attribute carrier attached to every data point.
type attributeSet = attribute.Set

func writeSampleWithExtra(w io.Writer, name string, attrs attributeSet, extraKey, extraVal string, value float64) {
	labels := renderLabels(attrs, extraKey, extraVal)
	fmt.Fprintf(w, "%s%s %s\n", name, labels, formatFloat(value))
}

func renderLabels(attrs attributeSet, extraKey, extraVal string) string {
	type kv struct{ k, v string }
	pairs := make([]kv, 0, attrs.Len()+1)
	iter := attrs.Iter()
	for iter.Next() {
		a := iter.Attribute()
		pairs = append(pairs, kv{sanitizeLabelName(string(a.Key)), a.Value.Emit()})
	}
	if extraKey != "" {
		pairs = append(pairs, kv{extraKey, extraVal})
	}
	if len(pairs) == 0 {
		return ""
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].k < pairs[j].k })
	var sb strings.Builder
	sb.WriteByte('{')
	for i, p := range pairs {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(p.k)
		sb.WriteString(`="`)
		sb.WriteString(escapeLabelValue(p.v))
		sb.WriteByte('"')
	}
	sb.WriteByte('}')
	return sb.String()
}

func formatFloat(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "+Inf"
	case math.IsInf(f, -1):
		return "-Inf"
	case math.IsNaN(f):
		return "NaN"
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

func sanitizeMetricName(s string) string { return sanitize(s, true) }
func sanitizeLabelName(s string) string  { return sanitize(s, false) }

func sanitize(s string, allowColon bool) string {
	var sb strings.Builder
	sb.Grow(len(s))
	for i, r := range s {
		switch {
		case unicode.IsLetter(r) || r == '_':
			sb.WriteRune(r)
		case allowColon && r == ':':
			sb.WriteRune(r)
		case unicode.IsDigit(r) && i > 0:
			sb.WriteRune(r)
		default:
			sb.WriteByte('_')
		}
	}
	return sb.String()
}

func escapeHelp(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, "\n", `\n`)
}

func escapeLabelValue(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return strings.ReplaceAll(s, "\n", `\n`)
}
