// Package execution is the REST adapter for the Execution Data Source.
//
// The EDS is the slow side of the system: workflow history, audit trails and
// multi-step execution records. Everything here is written on the assumption
// that this source will occasionally be slow enough to matter, so response
// bodies are size-limited, connections are pooled and reused, and the
// resilience stack around every call is non-negotiable.
//
// Traceability: REQ-DS-007..REQ-DS-012, REQ-RES-005, REQ-PERF-003.
package execution

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/udaykishore-resu/ttl-aware-bff/internal/config"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/datasource"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/domain"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/mapper"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/resilience"
	"github.com/udaykishore-resu/ttl-aware-bff/pkg/correlation"
	"github.com/udaykishore-resu/ttl-aware-bff/pkg/errs"
)

const sourceName = "EXECUTION"

// maxResponseBytes bounds how much the BFF will read from the EDS. A source
// that starts streaming an unbounded history must not be able to exhaust the
// BFF's memory (REQ-SEC-011).
const maxResponseBytes = 8 << 20

// Adapter implements datasource.ExecutionRepository over REST.
type Adapter struct {
	base   *url.URL
	client *http.Client
	exec   *resilience.Executor
	mapper *mapper.Execution
	cfg    config.ExecutionSourceConfig

	lastErr atomic.Pointer[string]
	// droppedRecords counts records dropped during page mapping, exposed for
	// the health surface so silent data loss is visible.
	droppedRecords atomic.Int64
}

// New builds the adapter with a tuned, connection-reusing transport.
func New(cfg config.ExecutionSourceConfig, exec *resilience.Executor, m *mapper.Execution) (*Adapter, error) {
	base, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("execution: parse base_url: %w", err)
	}

	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   cfg.CallTimeout.D(),
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:        cfg.MaxIdleConns,
		MaxIdleConnsPerHost: cfg.MaxIdleConnsPerHost,
		MaxConnsPerHost:     cfg.MaxIdleConnsPerHost * 2,
		IdleConnTimeout:     cfg.IdleConnTimeout.D(),
		TLSHandshakeTimeout: 5 * time.Second,
		ForceAttemptHTTP2:   true,
	}
	if cfg.TLS.Enabled {
		tc, err := buildTLS(cfg.TLS)
		if err != nil {
			return nil, err
		}
		tr.TLSClientConfig = tc
	}

	return &Adapter{
		base:   base,
		client: &http.Client{Transport: otelhttp.NewTransport(tr)},
		exec:   exec,
		mapper: m,
		cfg:    cfg,
	}, nil
}

// NewWithClient wraps an existing HTTP client, for tests against httptest.
func NewWithClient(baseURL string, c *http.Client, cfg config.ExecutionSourceConfig, exec *resilience.Executor, m *mapper.Execution) (*Adapter, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("execution: parse base_url: %w", err)
	}
	return &Adapter{base: base, client: c, exec: exec, mapper: m, cfg: cfg}, nil
}

// GetLatestExecution implements datasource.ExecutionRepository.
func (a *Adapter) GetLatestExecution(ctx context.Context, tenantID, resourceID string) (*domain.Execution, error) {
	var out *domain.Execution
	err := a.exec.Call(ctx, true, 0, func(ctx context.Context) error {
		var rec mapper.EDSExecution
		u := a.url("/eds/v1/resources/"+url.PathEscape(resourceID)+"/latest-execution", url.Values{
			"tenantId": {tenantID},
		})
		if err := a.get(ctx, u, tenantID, &rec, "GetLatestExecution"); err != nil {
			return err
		}
		mapped, mErr := a.mapper.One(&rec, tenantID)
		if mErr != nil {
			return mErr
		}
		out = mapped
		return nil
	})
	if err != nil {
		a.storeLastErr(err.Error())
		return nil, err
	}
	return out, nil
}

// GetExecution implements datasource.ExecutionRepository.
func (a *Adapter) GetExecution(ctx context.Context, tenantID, executionID string, opts datasource.ReadOptions) (*domain.Execution, error) {
	var out *domain.Execution
	err := a.exec.Call(ctx, true, opts.Timeout, func(ctx context.Context) error {
		q := url.Values{"tenantId": {tenantID}}
		// Audit records are only requested when the caller's RBAC grant allows
		// it, so an unprivileged caller never causes the audit trail to leave
		// the source at all (REQ-SEC-007).
		if opts.IncludeAudit {
			q.Set("includeAudit", "true")
		}
		if opts.IncludeSteps {
			q.Set("includeSteps", "true")
		}
		var rec mapper.EDSExecution
		if err := a.get(ctx, a.url("/eds/v1/executions/"+url.PathEscape(executionID), q), tenantID, &rec, "GetExecution"); err != nil {
			return err
		}
		mapped, mErr := a.mapper.One(&rec, tenantID)
		if mErr != nil {
			return mErr
		}
		out = mapped
		return nil
	})
	if err != nil {
		a.storeLastErr(err.Error())
		return nil, err
	}
	return out, nil
}

// ListExecutions implements datasource.ExecutionRepository.
func (a *Adapter) ListExecutions(ctx context.Context, tenantID, resourceID string, page datasource.PageRequest, opts datasource.ReadOptions) (*domain.ExecutionList, error) {
	limit := page.Limit
	if limit <= 0 {
		limit = a.cfg.HistoryPageSize
	}
	if a.cfg.MaxHistoryItems > 0 && limit > a.cfg.MaxHistoryItems {
		limit = a.cfg.MaxHistoryItems
	}

	var out *domain.ExecutionList
	err := a.exec.Call(ctx, true, opts.Timeout, func(ctx context.Context) error {
		q := url.Values{
			"tenantId":   {tenantID},
			"resourceId": {resourceID},
			"limit":      {strconv.Itoa(limit)},
		}
		if page.Cursor != "" {
			q.Set("cursor", page.Cursor)
		}
		if opts.IncludeAudit {
			q.Set("includeAudit", "true")
		}
		var pageResp mapper.EDSExecutionPage
		if err := a.get(ctx, a.url("/eds/v1/executions", q), tenantID, &pageResp, "ListExecutions"); err != nil {
			return err
		}
		list, dropped, mErr := a.mapper.Page(&pageResp, tenantID)
		if mErr != nil {
			return mErr
		}
		if dropped > 0 {
			a.droppedRecords.Add(int64(dropped))
		}
		if list.ResourceID == "" {
			list.ResourceID = resourceID
		}
		out = list
		return nil
	})
	if err != nil {
		a.storeLastErr(err.Error())
		return nil, err
	}
	return out, nil
}

// Health implements datasource.ExecutionRepository from local state.
func (a *Adapter) Health(context.Context) datasource.Health {
	return datasource.Health{
		Available: a.exec.Healthy(),
		Detail:    a.exec.HealthDetail(),
		LastError: a.loadLastErr(),
		CheckedAt: time.Now(),
	}
}

// Ping performs a real round trip against the source's health endpoint.
func (a *Adapter) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, a.cfg.CallTimeout.D())
	defer cancel()
	var body struct {
		Status string `json:"status"`
	}
	if err := a.get(ctx, a.url("/eds/v1/health", nil), "", &body, "Health"); err != nil {
		return err
	}
	if body.Status != "" && body.Status != "ok" && body.Status != "degraded" {
		return errs.New(errs.CodeUpstreamUnavailable, "execution source reports it is not serving").
			WithSource(sourceName)
	}
	return nil
}

// DroppedRecords reports how many malformed history records have been skipped.
func (a *Adapter) DroppedRecords() int64 { return a.droppedRecords.Load() }

// Close implements datasource.ExecutionRepository.
func (a *Adapter) Close() error {
	if t, ok := a.client.Transport.(interface{ CloseIdleConnections() }); ok {
		t.CloseIdleConnections()
	}
	return nil
}

// ---------------------------------------------------------------------------
// HTTP plumbing
// ---------------------------------------------------------------------------

func (a *Adapter) url(path string, q url.Values) string {
	u := *a.base
	u.Path = joinPath(u.Path, path)
	if len(q) > 0 {
		u.RawQuery = q.Encode()
	}
	return u.String()
}

func joinPath(base, p string) string {
	switch {
	case base == "" || base == "/":
		return p
	case base[len(base)-1] == '/':
		return base[:len(base)-1] + p
	default:
		return base + p
	}
}

// get performs one GET and decodes the JSON body into out.
func (a *Adapter) get(ctx context.Context, endpoint, tenantID string, out any, op string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return errs.Wrap(errs.CodeInternal, "could not build execution source request", err).
			WithSource(sourceName).WithOp(op)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set(correlation.HeaderCorrelationID, correlation.CorrelationID(ctx))
	if tenantID != "" {
		req.Header.Set(correlation.HeaderTenantID, tenantID)
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return a.classifyTransport(err, op)
	}
	defer func() {
		// Draining before closing lets the connection return to the pool
		// instead of being torn down, which matters for a source this chatty.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
	}()

	if err := a.classifyStatus(resp, op); err != nil {
		return err
	}

	body := io.LimitReader(resp.Body, maxResponseBytes)
	dec := json.NewDecoder(body)
	// A source that sends fields the BFF does not model is fine; a source that
	// sends a malformed document is not.
	if err := dec.Decode(out); err != nil {
		if errors.Is(err, io.EOF) {
			return errs.New(errs.CodeUpstreamInvalidPayload, "execution source returned an empty body").
				WithSource(sourceName).WithOp(op)
		}
		return errs.Wrap(errs.CodeUpstreamInvalidPayload, "execution source returned a malformed response", err).
			WithSource(sourceName).WithOp(op)
	}
	return nil
}

func (a *Adapter) classifyStatus(resp *http.Response, op string) error {
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusNotFound:
		return errs.New(errs.CodeNotFound, "execution record not found").WithSource(sourceName).WithOp(op)
	case resp.StatusCode == http.StatusUnauthorized:
		return errs.New(errs.CodeUnauthenticated, "execution source rejected the service credentials").
			WithSource(sourceName).WithOp(op)
	case resp.StatusCode == http.StatusForbidden:
		return errs.New(errs.CodeForbidden, "execution source denied access").WithSource(sourceName).WithOp(op)
	case resp.StatusCode == http.StatusTooManyRequests:
		e := errs.New(errs.CodeRateLimited, "execution source is rate limiting the BFF").
			WithSource(sourceName).WithOp(op)
		// Honour Retry-After as a hint for the retrier's logs; the retrier
		// itself uses its own bounded backoff.
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			e = e.WithDetail("retry_after", ra)
		}
		return e
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return errs.New(errs.CodeInvalidRequest, "execution source rejected the request").
			WithSource(sourceName).WithOp(op).WithDetail("status", resp.StatusCode)
	case resp.StatusCode == http.StatusGatewayTimeout:
		return errs.New(errs.CodeUpstreamTimeout, "execution source timed out upstream").
			WithSource(sourceName).WithOp(op)
	default:
		return errs.New(errs.CodeUpstreamUnavailable, "execution source returned a server error").
			WithSource(sourceName).WithOp(op).WithDetail("status", resp.StatusCode)
	}
}

func (a *Adapter) classifyTransport(err error, op string) error {
	switch {
	case errors.Is(err, context.Canceled):
		return errs.Wrap(errs.CodeUpstreamUnavailable, "execution source call was cancelled", err).
			WithSource(sourceName).WithOp(op).NotRetryable()
	case errors.Is(err, context.DeadlineExceeded):
		return errs.Wrap(errs.CodeUpstreamTimeout, "execution source did not respond in time", err).
			WithSource(sourceName).WithOp(op)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return errs.Wrap(errs.CodeUpstreamTimeout, "execution source did not respond in time", err).
			WithSource(sourceName).WithOp(op)
	}
	return errs.Wrap(errs.CodeUpstreamUnavailable, "execution source is unreachable", err).
		WithSource(sourceName).WithOp(op)
}

func (a *Adapter) storeLastErr(msg string) { a.lastErr.Store(&msg) }

func (a *Adapter) loadLastErr() string {
	if p := a.lastErr.Load(); p != nil {
		return *p
	}
	return ""
}

func buildTLS(cfg config.TLSConfig) (*tls.Config, error) {
	tc := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		ServerName:         cfg.ServerName,
		InsecureSkipVerify: cfg.InsecureSkipVerify, //nolint:gosec // operator-controlled; refused by config validation outside local
	}
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("execution: read CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("execution: CA file contains no usable certificates")
		}
		tc.RootCAs = pool
	}
	if cfg.CertFile != "" && cfg.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("execution: load client certificate: %w", err)
		}
		tc.Certificates = []tls.Certificate{cert}
	}
	return tc, nil
}
