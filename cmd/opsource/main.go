// Command opsource is a reference implementation of the Operational Data
// Source: a gRPC service holding current resource state in memory.
//
// It exists for three reasons:
//
//   - the compose stack and the integration tests need something real to talk
//     to, with the ODS's actual schema and semantics;
//   - the chaos controls on its admin port let the failure tests drive every
//     edge case the BFF claims to handle -- staleness, latency, unavailability,
//     partial records, clock skew, schema drift -- without patching code;
//   - it documents, executably, what the BFF expects a real ODS to provide,
//     in particular the cheap freshness probe.
//
// It is not production code and does not pretend to be: everything is in
// memory and nothing is durable.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	opsv1 "github.com/udaykishore/ttl-aware-bff/internal/datasource/operational/opsv1"
)

func main() {
	var (
		addr      = flag.String("addr", ":9101", "gRPC listen address")
		adminAddr = flag.String("admin-addr", ":9111", "admin/chaos HTTP listen address")
		seedCount = flag.Int("seed", 50, "number of seeded resources")
		logLevel  = flag.String("log-level", "info", "debug|info|warn|error")
	)
	flag.Parse()

	log := newLogger(*logLevel)
	srv := newServer(log, *seedCount)

	grpcServer := grpc.NewServer(
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionIdle: 5 * time.Minute,
			Time:              30 * time.Second,
			Timeout:           10 * time.Second,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	opsv1.RegisterOperationalServiceServer(grpcServer, srv)

	hs := health.NewServer()
	hs.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(grpcServer, hs)
	reflection.Register(grpcServer)

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Error("listen failed", slog.String("addr", *addr), slog.String("error", err.Error()))
		os.Exit(1)
	}

	admin := &http.Server{
		Addr:              *adminAddr,
		Handler:           srv.adminRoutes(),
		ReadHeaderTimeout: 2 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Info("operational source listening", slog.String("grpc", *addr), slog.Int("resources", *seedCount))
		if err := grpcServer.Serve(lis); err != nil {
			log.Error("gRPC server stopped", slog.String("error", err.Error()))
		}
	}()
	go func() {
		log.Info("operational source admin listening", slog.String("http", *adminAddr))
		if err := admin.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("admin server stopped", slog.String("error", err.Error()))
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = admin.Shutdown(shutdownCtx)
	grpcServer.GracefulStop()
}

// ---------------------------------------------------------------------------
// Server
// ---------------------------------------------------------------------------

type server struct {
	opsv1.UnimplementedOperationalServiceServer
	log *slog.Logger

	mu    sync.RWMutex
	store map[string]*opsv1.OperationalResource
	// ageOffset holds each record's age as a duration rather than as an
	// absolute timestamp.
	//
	// A real ODS is continuously refreshed by a poller, so a record's age
	// hovers around a steady value. Seeding absolute timestamps instead would
	// mean every record drifts monotonically into staleness the longer the
	// stub runs, and a developer who left the compose stack up over lunch
	// would come back to a BFF that routes everything to the execution source
	// for no visible reason. Storing the offset keeps the demonstration
	// stable: R00n always reports an age of (n%7)*5 seconds, so R007, R014 and
	// R021 are always fresh while R003 is always 15 seconds old.
	ageOffset map[string]time.Duration

	chaos chaos
}

// chaos holds the knobs the failure tests drive. Every one of them maps to a
// specific edge case in the BFF's requirements.
type chaos struct {
	mu sync.RWMutex

	// latency injected before every response.
	latencyMin, latencyMax time.Duration
	// failureRate in [0,1]: fraction of calls answered with UNAVAILABLE.
	failureRate float64
	// unavailable makes every call fail, simulating a total outage.
	unavailable bool
	// probeUnavailable fails only the freshness probe, so the BFF must apply
	// its unknown-freshness policy.
	probeUnavailable bool
	// staleBy ages every record's last_updated, forcing TTL misses.
	staleBy time.Duration
	// clockSkew shifts the reported server_time relative to real time.
	clockSkew time.Duration
	// partial drops optional fields from responses.
	partial bool
	// schemaVersion overrides the declared contract version.
	schemaVersion string
}

func newServer(log *slog.Logger, seed int) *server {
	s := &server{
		log:       log,
		store:     make(map[string]*opsv1.OperationalResource, seed),
		ageOffset: make(map[string]time.Duration, seed),
	}
	s.chaos.schemaVersion = "ods.v1"
	s.seed(seed)
	return s
}

func (s *server) seed(n int) {
	states := []opsv1.ResourceState{
		opsv1.ResourceState_RESOURCE_STATE_ACTIVE,
		opsv1.ResourceState_RESOURCE_STATE_PROVISIONING,
		opsv1.ResourceState_RESOURCE_STATE_DEGRADED,
		opsv1.ResourceState_RESOURCE_STATE_SUSPENDED,
	}
	now := time.Now()
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("R%03d", i)
		// Ages are spread across the seeded set on purpose, so a developer
		// running the compose stack sees both TTL hits and TTL misses without
		// touching the chaos knobs.
		age := time.Duration(i%7) * 5 * time.Second
		s.ageOffset[id] = age
		s.store[id] = &opsv1.OperationalResource{
			ResourceId:   id,
			CustomerRef:  fmt.Sprintf("C%03d", (i%9)+1),
			TenantId:     tenantFor(i),
			ResourceType: []string{"compute", "database", "queue", "gateway"}[i%4],
			State:        states[i%len(states)],
			Substate:     []string{"", "scaling", "reconciling", "healthy"}[i%4],
			Ownership: &opsv1.OwnershipRecord{
				OwnerId:    fmt.Sprintf("team-%d", (i%5)+1),
				OwnerType:  "team",
				OwnerEmail: fmt.Sprintf("team-%d@example.invalid", (i%5)+1),
				CostCentre: fmt.Sprintf("CC-%02d", (i%12)+1),
			},
			Configuration: map[string]string{
				"tier":     []string{"standard", "premium"}[i%2],
				"replicas": strconv.Itoa((i % 5) + 1),
				"region":   []string{"eu-west-1", "us-east-1"}[i%2],
			},
			Metrics: []*opsv1.MetricSample{
				{Name: "cpu_utilisation", Value: float64(20 + i%70), Unit: "percent", SampledAt: timestamppb.New(now)},
				{Name: "requests_per_second", Value: float64(100 + i*3), Unit: "rps", SampledAt: timestamppb.New(now)},
			},
			Topology: &opsv1.TopologyRecord{
				Region:         []string{"eu-west-1", "us-east-1"}[i%2],
				Zone:           fmt.Sprintf("%sa", []string{"eu-west-1", "us-east-1"}[i%2]),
				Cluster:        fmt.Sprintf("cluster-%d", (i%3)+1),
				UpstreamRefs:   []string{fmt.Sprintf("R%03d", max(1, i-1))},
				DownstreamRefs: []string{fmt.Sprintf("R%03d", i+1)},
			},
			OperationalMetadata: map[string]string{"managed_by": "reference-ods"},
			Labels:              map[string]string{"env": "local"},
			Freshness: &opsv1.FreshnessEnvelope{
				// Recomputed on every read from ageOffset; the value here is
				// only a sensible starting point.
				LastUpdated:   timestamppb.New(now.Add(-age)),
				ServerTime:    timestamppb.New(now),
				RefreshSource: "poller",
				Version:       uint64(i),
			},
			SchemaVersion: "ods.v1",
		}
		// The two reference sources are seeded from the same index arithmetic,
		// so the operational record's in-flight reference names an execution
		// the execution source really is reporting as running. That makes the
		// "a running execution may override operational state" precedence rule
		// observable in the compose stack without any manual setup.
		if i%5 == 1 {
			s.store[id].InFlightExecutionRef = fmt.Sprintf("E%03d-0", i)
		}
	}
}

// tenantFor spreads the seeded resources across tenants so tenant isolation is
// exercised by the ordinary demo data rather than only by a dedicated test.
func tenantFor(i int) string {
	switch i % 3 {
	case 0:
		return "acme"
	case 1:
		return "local"
	default:
		return "globex"
	}
}

// ---------------------------------------------------------------------------
// RPCs
// ---------------------------------------------------------------------------

func (s *server) GetResourceFreshness(ctx context.Context, req *opsv1.GetResourceFreshnessRequest) (*opsv1.GetResourceFreshnessResponse, error) {
	c := s.chaosSnapshot()
	if c.probeUnavailable {
		return nil, status.Error(codes.Unavailable, "freshness probe is unavailable")
	}
	// The probe is deliberately much cheaper than a read: a tenth of the
	// injected latency. A probe that costs as much as the read would make the
	// whole TTL routing design pointless.
	if err := s.delay(ctx, c, 10); err != nil {
		return nil, err
	}
	if err := s.maybeFail(c); err != nil {
		return nil, err
	}

	r, ok := s.get(req.GetResourceId(), req.GetContext().GetTenantId())
	if !ok {
		return &opsv1.GetResourceFreshnessResponse{ResourceId: req.GetResourceId(), Found: false}, nil
	}
	return &opsv1.GetResourceFreshnessResponse{
		ResourceId: r.GetResourceId(),
		Found:      true,
		Freshness:  s.freshnessFor(r, c),
	}, nil
}

func (s *server) GetResource(ctx context.Context, req *opsv1.GetResourceRequest) (*opsv1.GetResourceResponse, error) {
	c := s.chaosSnapshot()
	if err := s.delay(ctx, c, 1); err != nil {
		return nil, err
	}
	if err := s.maybeFail(c); err != nil {
		return nil, err
	}

	r, ok := s.get(req.GetResourceId(), req.GetContext().GetTenantId())
	if !ok {
		return nil, status.Errorf(codes.NotFound, "resource %s not found", req.GetResourceId())
	}
	return &opsv1.GetResourceResponse{Resource: s.render(r, c)}, nil
}

func (s *server) GetResourceState(ctx context.Context, req *opsv1.GetResourceStateRequest) (*opsv1.GetResourceStateResponse, error) {
	c := s.chaosSnapshot()
	if err := s.delay(ctx, c, 2); err != nil {
		return nil, err
	}
	if err := s.maybeFail(c); err != nil {
		return nil, err
	}

	r, ok := s.get(req.GetResourceId(), req.GetContext().GetTenantId())
	if !ok {
		return nil, status.Errorf(codes.NotFound, "resource %s not found", req.GetResourceId())
	}
	return &opsv1.GetResourceStateResponse{
		ResourceId:           r.GetResourceId(),
		State:                r.GetState(),
		Substate:             r.GetSubstate(),
		Freshness:            s.freshnessFor(r, c),
		InFlightExecutionRef: r.GetInFlightExecutionRef(),
	}, nil
}

func (s *server) BatchGetResources(ctx context.Context, req *opsv1.BatchGetResourcesRequest) (*opsv1.BatchGetResourcesResponse, error) {
	c := s.chaosSnapshot()
	if err := s.delay(ctx, c, 1); err != nil {
		return nil, err
	}
	if err := s.maybeFail(c); err != nil {
		return nil, err
	}

	out := &opsv1.BatchGetResourcesResponse{}
	for _, id := range req.GetResourceIds() {
		if r, ok := s.get(id, req.GetContext().GetTenantId()); ok {
			out.Resources = append(out.Resources, s.render(r, c))
			continue
		}
		out.MissingResourceIds = append(out.MissingResourceIds, id)
	}
	return out, nil
}

func (s *server) Health(context.Context, *opsv1.HealthRequest) (*opsv1.HealthResponse, error) {
	c := s.chaosSnapshot()
	switch {
	case c.unavailable:
		return &opsv1.HealthResponse{State: opsv1.HealthState_HEALTH_STATE_NOT_SERVING, Detail: "chaos: unavailable"}, nil
	case c.failureRate > 0:
		return &opsv1.HealthResponse{State: opsv1.HealthState_HEALTH_STATE_DEGRADED, Detail: "chaos: injected failures"}, nil
	default:
		return &opsv1.HealthResponse{State: opsv1.HealthState_HEALTH_STATE_SERVING}, nil
	}
}

// ---------------------------------------------------------------------------
// Rendering and chaos
// ---------------------------------------------------------------------------

// get enforces tenant isolation at the source, which is what lets the BFF's
// own tenant checks be defence in depth rather than the only line.
func (s *server) get(id, tenant string) (*opsv1.OperationalResource, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.store[id]
	if !ok {
		return nil, false
	}
	if tenant != "" && r.GetTenantId() != tenant {
		return nil, false
	}
	return r, true
}

func (s *server) render(r *opsv1.OperationalResource, c chaosSnapshot) *opsv1.OperationalResource {
	out := &opsv1.OperationalResource{
		ResourceId:           r.GetResourceId(),
		CustomerRef:          r.GetCustomerRef(),
		TenantId:             r.GetTenantId(),
		ResourceType:         r.GetResourceType(),
		State:                r.GetState(),
		Substate:             r.GetSubstate(),
		Configuration:        r.GetConfiguration(),
		Freshness:            s.freshnessFor(r, c),
		InFlightExecutionRef: r.GetInFlightExecutionRef(),
		SchemaVersion:        c.schemaVersion,
	}
	if c.partial {
		// A partially populated record: the BFF must produce a usable answer
		// and warn, rather than failing (REQ-EDGE-006).
		return out
	}
	out.Ownership = r.GetOwnership()
	out.Metrics = r.GetMetrics()
	out.Topology = r.GetTopology()
	out.OperationalMetadata = r.GetOperationalMetadata()
	out.Labels = r.GetLabels()
	return out
}

// freshnessFor computes the record's freshness envelope at read time.
//
// Age is derived from the stored offset plus the injected staleness, so the
// record's apparent age is whatever the offset says regardless of how long the
// stub has been running. server_time carries the injected clock skew, which is
// what lets a test drive the BFF's skew correction.
func (s *server) freshnessFor(r *opsv1.OperationalResource, c chaosSnapshot) *opsv1.FreshnessEnvelope {
	f := r.GetFreshness()
	now := time.Now()

	s.mu.RLock()
	offset := s.ageOffset[r.GetResourceId()]
	s.mu.RUnlock()

	return &opsv1.FreshnessEnvelope{
		LastUpdated:   timestamppb.New(now.Add(-offset).Add(-c.staleBy)),
		ServerTime:    timestamppb.New(now.Add(c.clockSkew)),
		RefreshSource: f.GetRefreshSource(),
		Version:       f.GetVersion(),
	}
}

type chaosSnapshot struct {
	latencyMin, latencyMax time.Duration
	failureRate            float64
	unavailable            bool
	probeUnavailable       bool
	staleBy                time.Duration
	clockSkew              time.Duration
	partial                bool
	schemaVersion          string
}

func (s *server) chaosSnapshot() chaosSnapshot {
	s.chaos.mu.RLock()
	defer s.chaos.mu.RUnlock()
	return chaosSnapshot{
		latencyMin: s.chaos.latencyMin, latencyMax: s.chaos.latencyMax,
		failureRate: s.chaos.failureRate, unavailable: s.chaos.unavailable,
		probeUnavailable: s.chaos.probeUnavailable, staleBy: s.chaos.staleBy,
		clockSkew: s.chaos.clockSkew, partial: s.chaos.partial,
		schemaVersion: s.chaos.schemaVersion,
	}
}

// delay injects latency, divided by weight so the probe stays cheap.
func (s *server) delay(ctx context.Context, c chaosSnapshot, weight int) error {
	if c.latencyMax <= 0 || weight <= 0 {
		return nil
	}
	d := c.latencyMin
	if c.latencyMax > c.latencyMin {
		d += time.Duration(rand.Int64N(int64(c.latencyMax - c.latencyMin)))
	}
	d /= time.Duration(weight)
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return status.FromContextError(ctx.Err()).Err()
	case <-t.C:
		return nil
	}
}

func (s *server) maybeFail(c chaosSnapshot) error {
	if c.unavailable {
		return status.Error(codes.Unavailable, "operational source is unavailable (chaos)")
	}
	if c.failureRate > 0 && rand.Float64() < c.failureRate {
		return status.Error(codes.Unavailable, "operational source transient failure (chaos)")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Admin surface
// ---------------------------------------------------------------------------

func (s *server) adminRoutes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// The compose stack and the Kubernetes manifests probe /readyz and /livez,
	// so the reference sources answer the same three probes the BFF does.
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "alive"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})

	mux.HandleFunc("GET /chaos", func(w http.ResponseWriter, _ *http.Request) {
		c := s.chaosSnapshot()
		writeJSON(w, http.StatusOK, map[string]any{
			"latency_min_ms":     c.latencyMin.Milliseconds(),
			"latency_max_ms":     c.latencyMax.Milliseconds(),
			"failure_rate":       c.failureRate,
			"unavailable":        c.unavailable,
			"probe_unavailable":  c.probeUnavailable,
			"stale_by_seconds":   c.staleBy.Seconds(),
			"clock_skew_seconds": c.clockSkew.Seconds(),
			"partial":            c.partial,
			"schema_version":     c.schemaVersion,
		})
	})

	// PUT /chaos accepts any subset of the knobs, so a test can change one
	// dimension without restating the rest.
	mux.HandleFunc("PUT /chaos", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			LatencyMinMS     *int64   `json:"latency_min_ms"`
			LatencyMaxMS     *int64   `json:"latency_max_ms"`
			FailureRate      *float64 `json:"failure_rate"`
			Unavailable      *bool    `json:"unavailable"`
			ProbeUnavailable *bool    `json:"probe_unavailable"`
			StaleBySeconds   *float64 `json:"stale_by_seconds"`
			ClockSkewSeconds *float64 `json:"clock_skew_seconds"`
			Partial          *bool    `json:"partial"`
			SchemaVersion    *string  `json:"schema_version"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		s.chaos.mu.Lock()
		if body.LatencyMinMS != nil {
			s.chaos.latencyMin = time.Duration(*body.LatencyMinMS) * time.Millisecond
		}
		if body.LatencyMaxMS != nil {
			s.chaos.latencyMax = time.Duration(*body.LatencyMaxMS) * time.Millisecond
		}
		if body.FailureRate != nil {
			s.chaos.failureRate = *body.FailureRate
		}
		if body.Unavailable != nil {
			s.chaos.unavailable = *body.Unavailable
		}
		if body.ProbeUnavailable != nil {
			s.chaos.probeUnavailable = *body.ProbeUnavailable
		}
		if body.StaleBySeconds != nil {
			s.chaos.staleBy = time.Duration(*body.StaleBySeconds * float64(time.Second))
		}
		if body.ClockSkewSeconds != nil {
			s.chaos.clockSkew = time.Duration(*body.ClockSkewSeconds * float64(time.Second))
		}
		if body.Partial != nil {
			s.chaos.partial = *body.Partial
		}
		if body.SchemaVersion != nil {
			s.chaos.schemaVersion = *body.SchemaVersion
		}
		s.chaos.mu.Unlock()
		s.log.Info("chaos configuration updated")
		writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
	})

	mux.HandleFunc("DELETE /chaos", func(w http.ResponseWriter, _ *http.Request) {
		s.chaos.mu.Lock()
		// Reset field by field. Assigning a fresh struct would overwrite the
		// very mutex being held, and the following Unlock would then panic on
		// an unlocked mutex -- taking the process down.
		s.chaos.latencyMin, s.chaos.latencyMax = 0, 0
		s.chaos.failureRate = 0
		s.chaos.unavailable = false
		s.chaos.probeUnavailable = false
		s.chaos.staleBy = 0
		s.chaos.clockSkew = 0
		s.chaos.partial = false
		s.chaos.schemaVersion = "ods.v1"
		s.chaos.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]string{"status": "reset"})
	})

	// Touch a resource, so a test can prove that a refreshed record flips the
	// BFF from a fallback route back to the operational route.
	mux.HandleFunc("POST /resources/{id}/touch", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		s.mu.Lock()
		defer s.mu.Unlock()
		rec, ok := s.store[id]
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		s.ageOffset[id] = 0
		rec.Freshness.Version++
		writeJSON(w, http.StatusOK, map[string]any{"resourceId": id, "version": rec.Freshness.Version, "age_seconds": 0})
	})

	// Age a single resource, for TTL tests that must not disturb others.
	mux.HandleFunc("POST /resources/{id}/age", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		secs, _ := strconv.ParseFloat(r.URL.Query().Get("seconds"), 64)
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, ok := s.store[id]; !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		s.ageOffset[id] += time.Duration(secs * float64(time.Second))
		writeJSON(w, http.StatusOK, map[string]any{
			"resourceId": id, "age_seconds": s.ageOffset[id].Seconds()})
	})

	// Mark a resource as being mutated by a workflow, which is the trigger for
	// the execution-overrides-operational precedence rule.
	mux.HandleFunc("POST /resources/{id}/in-flight", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		exec := r.URL.Query().Get("executionId")
		s.mu.Lock()
		defer s.mu.Unlock()
		rec, ok := s.store[id]
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		rec.InFlightExecutionRef = exec
		writeJSON(w, http.StatusOK, map[string]any{"resourceId": id, "inFlightExecutionRef": exec})
	})

	mux.HandleFunc("GET /resources", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.RLock()
		defer s.mu.RUnlock()
		ids := make([]string, 0, len(s.store))
		for id := range s.store {
			ids = append(ids, id)
		}
		writeJSON(w, http.StatusOK, map[string]any{"count": len(ids), "resourceIds": ids})
	})

	return mux
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func newLogger(level string) *slog.Logger {
	l := slog.LevelInfo
	switch level {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: l}))
}
