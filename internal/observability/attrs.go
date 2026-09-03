package observability

import (
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// MetricAttrs is a small helper so call sites read as
// counter.Add(ctx, 1, observability.MetricAttrs(...)) rather than repeating
// metric.WithAttributes at every use.
func MetricAttrs(attrs ...attribute.KeyValue) metric.MeasurementOption {
	return metric.WithAttributes(attrs...)
}
