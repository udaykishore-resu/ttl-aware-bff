// Package operational is the gRPC adapter for the Operational Data Source.
//
// It is the only package in the service that knows the ODS speaks gRPC. Above
// it, the application layer sees datasource.OperationalRepository and canonical
// domain types.
//
// Traceability: REQ-DS-001..REQ-DS-006, REQ-RES-001, REQ-MT-002.
package operational

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/udaykishore-resu/ttl-aware-bff/internal/config"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/datasource"
	opsv1 "github.com/udaykishore-resu/ttl-aware-bff/internal/datasource/operational/opsv1"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/domain"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/mapper"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/resilience"
	"github.com/udaykishore-resu/ttl-aware-bff/pkg/correlation"
	"github.com/udaykishore-resu/ttl-aware-bff/pkg/errs"
)

// sourceName is the label used in errors, metrics and breaker identity.
const sourceName = "OPERATIONAL"

// Adapter implements datasource.OperationalRepository over gRPC.
type Adapter struct {
	conn   *grpc.ClientConn
	client opsv1.OperationalServiceClient
	exec   *resilience.Executor
	mapper *mapper.Operational
	cfg    config.OperationalSourceConfig

	// lastErr is read by the health endpoint from another goroutine, so it is
	// held atomically rather than as a plain field.
	lastErr atomic.Pointer[string]
}

func (a *Adapter) storeLastErr(msg string) { a.lastErr.Store(&msg) }

func (a *Adapter) loadLastErr() string {
	if p := a.lastErr.Load(); p != nil {
		return *p
	}
	return ""
}

// tsOf converts a protobuf timestamp, treating an unset or invalid value as
// the zero time rather than as the epoch.
func tsOf(p *timestamppb.Timestamp) time.Time {
	if p == nil || !p.IsValid() {
		return time.Time{}
	}
	return p.AsTime()
}

// New dials the ODS and returns the adapter.
//
// The dial is non-blocking: gRPC connects lazily and reconnects on its own, so
// a source that is down at boot must not stop the BFF from starting. The
// readiness probe reports the source's state separately.
func New(cfg config.OperationalSourceConfig, exec *resilience.Executor, m *mapper.Operational) (*Adapter, error) {
	creds, err := buildCredentials(cfg.TLS)
	if err != nil {
		return nil, err
	}

	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                cfg.KeepaliveTime.D(),
			Timeout:             cfg.KeepaliveTimeout.D(),
			PermitWithoutStream: true,
		}),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(cfg.MaxRecvMsgBytes),
		),
	}
	if cfg.UseRoundRobin {
		// Round-robin over every resolved backend keeps a single hot instance
		// from absorbing the whole read load, and it is what makes the
		// per-source bulkhead a meaningful bound.
		opts = append(opts, grpc.WithDefaultServiceConfig(`{"loadBalancingConfig":[{"round_robin":{}}]}`))
	}

	conn, err := grpc.NewClient(cfg.Addr, opts...)
	if err != nil {
		return nil, fmt.Errorf("operational: dial %s: %w", cfg.Addr, err)
	}
	return &Adapter{
		conn:   conn,
		client: opsv1.NewOperationalServiceClient(conn),
		exec:   exec,
		mapper: m,
		cfg:    cfg,
	}, nil
}

// NewWithClient wraps an existing client. Used by tests against a bufconn
// server and by the contract tests.
func NewWithClient(client opsv1.OperationalServiceClient, cfg config.OperationalSourceConfig, exec *resilience.Executor, m *mapper.Operational) *Adapter {
	return &Adapter{client: client, exec: exec, mapper: m, cfg: cfg}
}

// ProbeFreshness implements datasource.FreshnessProbe.
//
// This call has its own, much tighter timeout than a full read. That is the
// whole point of the probe: it must be cheap enough that asking "how old is
// this?" never costs more than reading the record would have.
func (a *Adapter) ProbeFreshness(ctx context.Context, tenantID, resourceID string) (datasource.Observation, error) {
	var out datasource.Observation
	err := a.exec.Call(ctx, true, a.cfg.ProbeTimeout.D(), func(ctx context.Context) error {
		resp, err := a.client.GetResourceFreshness(a.outgoing(ctx, tenantID), &opsv1.GetResourceFreshnessRequest{
			Context:    a.reqContext(ctx, tenantID),
			ResourceId: resourceID,
		})
		if err != nil {
			return a.classify(err, "ProbeFreshness")
		}
		if !resp.GetFound() {
			out = datasource.Observation{Found: false}
			return nil
		}
		f := resp.GetFreshness()
		out = datasource.Observation{
			Found:         true,
			LastUpdated:   tsOf(f.GetLastUpdated()),
			SourceTime:    tsOf(f.GetServerTime()),
			Version:       f.GetVersion(),
			RefreshSource: f.GetRefreshSource(),
		}
		return nil
	})
	if err != nil {
		a.storeLastErr(err.Error())
		return datasource.Observation{}, err
	}
	return out, nil
}

// GetResource implements datasource.OperationalRepository.
func (a *Adapter) GetResource(ctx context.Context, tenantID, resourceID string, opts datasource.ReadOptions) (*domain.Resource, domain.Freshness, error) {
	var (
		res *domain.Resource
		fr  domain.Freshness
	)
	err := a.exec.Call(ctx, true, opts.Timeout, func(ctx context.Context) error {
		resp, err := a.client.GetResource(a.outgoing(ctx, tenantID), &opsv1.GetResourceRequest{
			Context:    a.reqContext(ctx, tenantID),
			ResourceId: resourceID,
			FieldMask:  opts.Fields,
		})
		if err != nil {
			return a.classify(err, "GetResource")
		}
		r, f, mErr := a.mapper.Resource(resp.GetResource(), tenantID)
		if mErr != nil {
			return mErr
		}
		res, fr = r, f
		return nil
	})
	if err != nil {
		a.storeLastErr(err.Error())
		return nil, domain.Freshness{}, err
	}
	return res, fr, nil
}

// GetResourceState implements datasource.OperationalRepository.
func (a *Adapter) GetResourceState(ctx context.Context, tenantID, resourceID string) (*domain.Resource, domain.Freshness, error) {
	var (
		res *domain.Resource
		fr  domain.Freshness
	)
	err := a.exec.Call(ctx, true, 0, func(ctx context.Context) error {
		resp, err := a.client.GetResourceState(a.outgoing(ctx, tenantID), &opsv1.GetResourceStateRequest{
			Context:    a.reqContext(ctx, tenantID),
			ResourceId: resourceID,
		})
		if err != nil {
			return a.classify(err, "GetResourceState")
		}
		r, f, mErr := a.mapper.State(resp, tenantID)
		if mErr != nil {
			return mErr
		}
		res, fr = r, f
		return nil
	})
	if err != nil {
		a.storeLastErr(err.Error())
		return nil, domain.Freshness{}, err
	}
	return res, fr, nil
}

// BatchGetResources implements datasource.OperationalRepository.
func (a *Adapter) BatchGetResources(ctx context.Context, tenantID string, resourceIDs []string) ([]domain.Resource, error) {
	if len(resourceIDs) == 0 {
		return nil, nil
	}
	var out []domain.Resource
	err := a.exec.Call(ctx, true, 0, func(ctx context.Context) error {
		resp, err := a.client.BatchGetResources(a.outgoing(ctx, tenantID), &opsv1.BatchGetResourcesRequest{
			Context:     a.reqContext(ctx, tenantID),
			ResourceIds: resourceIDs,
		})
		if err != nil {
			return a.classify(err, "BatchGetResources")
		}
		out = make([]domain.Resource, 0, len(resp.GetResources()))
		for _, r := range resp.GetResources() {
			mapped, _, mErr := a.mapper.Resource(r, tenantID)
			if mErr != nil {
				// A tenant mismatch anywhere in a batch invalidates the batch.
				if errs.CodeOf(mErr) == errs.CodeTenantMismatch {
					return mErr
				}
				continue
			}
			out = append(out, *mapped)
		}
		return nil
	})
	if err != nil {
		a.storeLastErr(err.Error())
		return nil, err
	}
	return out, nil
}

// Health implements datasource.OperationalRepository. It reports the locally
// observed state -- breaker and bulkhead -- rather than making a network call,
// because the router consults it on every request and a probe per request
// would defeat the purpose.
func (a *Adapter) Health(context.Context) datasource.Health {
	detail := a.exec.HealthDetail()
	return datasource.Health{
		Available: a.exec.Healthy(),
		Detail:    detail,
		LastError: a.loadLastErr(),
		CheckedAt: time.Now(),
	}
}

// Ping performs a real round trip. Used by the readiness probe and by
// integration tests, never on the request path.
func (a *Adapter) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, a.cfg.DialTimeout.D())
	defer cancel()
	resp, err := a.client.Health(ctx, &opsv1.HealthRequest{})
	if err != nil {
		return a.classify(err, "Health")
	}
	if resp.GetState() == opsv1.HealthState_HEALTH_STATE_NOT_SERVING {
		return errs.New(errs.CodeUpstreamUnavailable, "operational source reports it is not serving").
			WithSource(sourceName)
	}
	return nil
}

// Close implements datasource.OperationalRepository.
func (a *Adapter) Close() error {
	if a.conn == nil {
		return nil
	}
	return a.conn.Close()
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// outgoing attaches tenant and correlation metadata to the gRPC call. The
// tenant travels in metadata as well as in the request body so the source can
// enforce isolation at its edge (REQ-MT-002).
func (a *Adapter) outgoing(ctx context.Context, tenantID string) context.Context {
	md := metadata.Pairs(
		correlation.MetadataTenantID, tenantID,
		correlation.MetadataCorrelationID, correlation.CorrelationID(ctx),
	)
	return metadata.NewOutgoingContext(ctx, md)
}

func (a *Adapter) reqContext(ctx context.Context, tenantID string) *opsv1.RequestContext {
	return &opsv1.RequestContext{
		TenantId:      tenantID,
		CorrelationId: correlation.CorrelationID(ctx),
		Principal:     correlation.Principal(ctx),
	}
}

// classify converts a gRPC status into the service's error taxonomy.
//
// The mapping is deliberate about which codes count as evidence that the
// source is unwell: NOT_FOUND and INVALID_ARGUMENT are the caller's problem
// and must never trip a circuit breaker, while UNAVAILABLE and
// DEADLINE_EXCEEDED are exactly that evidence.
func (a *Adapter) classify(err error, op string) error {
	if err == nil {
		return nil
	}
	st, ok := status.FromError(err)
	if !ok {
		if errors.Is(err, context.DeadlineExceeded) {
			return errs.Wrap(errs.CodeUpstreamTimeout, "operational source timed out", err).
				WithSource(sourceName).WithOp(op)
		}
		return errs.Wrap(errs.CodeUpstreamUnavailable, "operational source call failed", err).
			WithSource(sourceName).WithOp(op)
	}

	switch st.Code() {
	case codes.OK:
		return nil
	case codes.NotFound:
		return errs.New(errs.CodeNotFound, "resource not found").WithSource(sourceName).WithOp(op)
	case codes.InvalidArgument, codes.FailedPrecondition, codes.OutOfRange:
		return errs.New(errs.CodeInvalidRequest, "operational source rejected the request").
			WithSource(sourceName).WithOp(op)
	case codes.PermissionDenied:
		return errs.New(errs.CodeForbidden, "operational source denied access").WithSource(sourceName).WithOp(op)
	case codes.Unauthenticated:
		return errs.New(errs.CodeUnauthenticated, "operational source rejected the service credentials").
			WithSource(sourceName).WithOp(op)
	case codes.DeadlineExceeded:
		return errs.Wrap(errs.CodeUpstreamTimeout, "operational source did not respond in time", err).
			WithSource(sourceName).WithOp(op)
	case codes.ResourceExhausted:
		return errs.Wrap(errs.CodeRateLimited, "operational source is rate limiting the BFF", err).
			WithSource(sourceName).WithOp(op)
	case codes.Unavailable, codes.Aborted:
		return errs.Wrap(errs.CodeUpstreamUnavailable, "operational source is unavailable", err).
			WithSource(sourceName).WithOp(op)
	case codes.Canceled:
		return errs.Wrap(errs.CodeUpstreamUnavailable, "operational source call was cancelled", err).
			WithSource(sourceName).WithOp(op).NotRetryable()
	case codes.Unimplemented:
		// A method the source does not implement is a contract problem, and
		// retrying it forever would hide a deployment mismatch.
		return errs.Wrap(errs.CodeSchemaVersionMismatch, "operational source does not implement this method", err).
			WithSource(sourceName).WithOp(op)
	default:
		return errs.Wrap(errs.CodeUpstreamUnavailable, "operational source returned an unexpected error", err).
			WithSource(sourceName).WithOp(op)
	}
}

func buildCredentials(cfg config.TLSConfig) (credentials.TransportCredentials, error) {
	if !cfg.Enabled {
		return insecure.NewCredentials(), nil
	}
	tc := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		ServerName:         cfg.ServerName,
		InsecureSkipVerify: cfg.InsecureSkipVerify, //nolint:gosec // operator-controlled; refused by config validation outside local
	}
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("operational: read CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("operational: CA file contains no usable certificates")
		}
		tc.RootCAs = pool
	}
	if cfg.CertFile != "" && cfg.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("operational: load client certificate: %w", err)
		}
		tc.Certificates = []tls.Certificate{cert}
	}
	return credentials.NewTLS(tc), nil
}
