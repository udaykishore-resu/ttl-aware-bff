package observability

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/udaykishore-resu/ttl-aware-bff/internal/config"
)

// InstrumentationName is the scope attached to every span and instrument.
const InstrumentationName = "github.com/udaykishore-resu/ttl-aware-bff"

// Provider owns the OpenTelemetry pipelines and the service's metric
// instruments. One is created at startup and shut down on exit.
type Provider struct {
	Tracer  trace.Tracer
	Meter   metric.Meter
	Metrics *Metrics

	// PromReader is a manual reader used by the in-process /metrics endpoint.
	// It is nil when the Prometheus endpoint is disabled.
	PromReader *sdkmetric.ManualReader

	tracerProvider *sdktrace.TracerProvider
	meterProvider  *sdkmetric.MeterProvider
}

// NewProvider builds the telemetry pipelines.
//
// Design notes:
//
//   - The OTLP exporters are optional. With observability.otlp.enabled=false
//     the service still records metrics (for the local /metrics endpoint) and
//     still propagates trace context, it simply exports nothing. That keeps
//     unit and integration tests free of a collector dependency.
//   - Sampling is parent-based on top of a ratio sampler, so a sampled
//     upstream trace is never dropped halfway through the BFF.
func NewProvider(ctx context.Context, cfg config.ObservabilityConfig) (*Provider, error) {
	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceVersion(cfg.ServiceVersion),
			semconv.DeploymentEnvironmentName(cfg.Environment),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("observability: build resource: %w", err)
	}

	p := &Provider{}

	// ---- traces -----------------------------------------------------------
	sampler := sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.TraceSampleRatio))
	tpOpts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
	}
	if cfg.OTLP.Enabled {
		exp, err := newTraceExporter(ctx, cfg.OTLP)
		if err != nil {
			return nil, err
		}
		tpOpts = append(tpOpts, sdktrace.WithBatcher(exp,
			sdktrace.WithMaxQueueSize(4096),
			sdktrace.WithMaxExportBatchSize(512),
			sdktrace.WithBatchTimeout(5*time.Second),
		))
	}
	p.tracerProvider = sdktrace.NewTracerProvider(tpOpts...)

	// ---- metrics ----------------------------------------------------------
	mpOpts := []sdkmetric.Option{sdkmetric.WithResource(res)}
	if cfg.OTLP.Enabled {
		exp, err := newMetricExporter(ctx, cfg.OTLP)
		if err != nil {
			return nil, err
		}
		interval := cfg.MetricsInterval.D()
		if interval <= 0 {
			interval = 15 * time.Second
		}
		mpOpts = append(mpOpts, sdkmetric.WithReader(
			sdkmetric.NewPeriodicReader(exp, sdkmetric.WithInterval(interval)),
		))
	}
	if cfg.PrometheusEndpoint {
		p.PromReader = sdkmetric.NewManualReader()
		mpOpts = append(mpOpts, sdkmetric.WithReader(p.PromReader))
	}
	// Latency histograms need buckets appropriate to a BFF: sub-millisecond
	// resolution at the fast end (the operational source is meant to be fast)
	// and coverage out to the execution source's multi-second tail.
	mpOpts = append(mpOpts, sdkmetric.WithView(latencyView()))
	p.meterProvider = sdkmetric.NewMeterProvider(mpOpts...)

	otel.SetTracerProvider(p.tracerProvider)
	otel.SetMeterProvider(p.meterProvider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	p.Tracer = p.tracerProvider.Tracer(InstrumentationName)
	p.Meter = p.meterProvider.Meter(InstrumentationName)

	m, err := NewMetrics(p.Meter)
	if err != nil {
		return nil, err
	}
	p.Metrics = m
	return p, nil
}

// NewNoopProvider returns a Provider that records nothing. Tests that do not
// assert on telemetry use it to avoid setting up exporters.
func NewNoopProvider() *Provider {
	mp := sdkmetric.NewMeterProvider()
	meter := mp.Meter(InstrumentationName)
	m, err := NewMetrics(meter)
	if err != nil {
		// NewMetrics only fails on a programming error in instrument names.
		panic(fmt.Sprintf("observability: noop provider: %v", err))
	}
	return &Provider{
		Tracer:        noop.NewTracerProvider().Tracer(InstrumentationName),
		Meter:         meter,
		Metrics:       m,
		meterProvider: mp,
	}
}

func latencyView() sdkmetric.View {
	buckets := []float64{
		0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.075,
		0.1, 0.15, 0.2, 0.3, 0.4, 0.5, 0.75, 1, 1.5, 2, 3, 5, 8, 13,
	}
	return sdkmetric.NewView(
		sdkmetric.Instrument{Kind: sdkmetric.InstrumentKindHistogram},
		sdkmetric.Stream{
			Aggregation: sdkmetric.AggregationExplicitBucketHistogram{
				Boundaries: buckets,
				NoMinMax:   false,
			},
		},
	)
}

func newTraceExporter(ctx context.Context, cfg config.OTLPConfig) (*otlptrace.Exporter, error) {
	opts := []otlptracegrpc.Option{
		otlptracegrpc.WithEndpoint(cfg.Endpoint),
		otlptracegrpc.WithTimeout(cfg.Timeout.D()),
	}
	if cfg.Insecure {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}
	if len(cfg.Headers) > 0 {
		opts = append(opts, otlptracegrpc.WithHeaders(cfg.Headers))
	}
	exp, err := otlptracegrpc.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("observability: otlp trace exporter: %w", err)
	}
	return exp, nil
}

func newMetricExporter(ctx context.Context, cfg config.OTLPConfig) (sdkmetric.Exporter, error) {
	opts := []otlpmetricgrpc.Option{
		otlpmetricgrpc.WithEndpoint(cfg.Endpoint),
		otlpmetricgrpc.WithTimeout(cfg.Timeout.D()),
	}
	if cfg.Insecure {
		opts = append(opts, otlpmetricgrpc.WithInsecure())
	}
	if len(cfg.Headers) > 0 {
		opts = append(opts, otlpmetricgrpc.WithHeaders(cfg.Headers))
	}
	exp, err := otlpmetricgrpc.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("observability: otlp metric exporter: %w", err)
	}
	return exp, nil
}

// Shutdown flushes and stops both pipelines. It is safe to call more than once.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p == nil {
		return nil
	}
	var errs []error
	if p.tracerProvider != nil {
		if err := p.tracerProvider.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("trace provider: %w", err))
		}
	}
	if p.meterProvider != nil {
		if err := p.meterProvider.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("meter provider: %w", err))
		}
	}
	return errors.Join(errs...)
}

// StartSpan is a thin helper so call sites do not repeat the scope name.
func (p *Provider) StartSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	if p == nil || p.Tracer == nil {
		return ctx, noop.Span{}
	}
	return p.Tracer.Start(ctx, name, trace.WithAttributes(attrs...))
}
