// Command exsource is a reference implementation of the Execution Data
// Source: a REST service holding workflow, history and audit records.
//
// It is deliberately slower than the operational source by default. That is
// not a limitation of the stub, it is the property the whole BFF design exists
// to work around, and having it present locally means a developer sees the
// latency difference the routing policy is trading against.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/udaykishore/ttl-aware-bff/internal/mapper"
)

func main() {
	var (
		addr        = flag.String("addr", ":9102", "HTTP listen address")
		adminAddr   = flag.String("admin-addr", ":9112", "admin/chaos HTTP listen address")
		seedCount   = flag.Int("seed", 50, "number of resources to seed executions for")
		baseLatency = flag.Duration("base-latency", 120*time.Millisecond, "baseline latency, reflecting a history-oriented store")
		logLevel    = flag.String("log-level", "info", "debug|info|warn|error")
	)
	flag.Parse()

	log := newLogger(*logLevel)
	srv := newServer(log, *seedCount, *baseLatency)

	api := &http.Server{
		Addr:              *addr,
		Handler:           srv.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	admin := &http.Server{
		Addr:              *adminAddr,
		Handler:           srv.adminRoutes(),
		ReadHeaderTimeout: 2 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Info("execution source listening",
			slog.String("http", *addr), slog.Duration("base_latency", *baseLatency))
		if err := api.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("api server stopped", slog.String("error", err.Error()))
		}
	}()
	go func() {
		log.Info("execution source admin listening", slog.String("http", *adminAddr))
		if err := admin.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("admin server stopped", slog.String("error", err.Error()))
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = api.Shutdown(shutdownCtx)
	_ = admin.Shutdown(shutdownCtx)
}

// ---------------------------------------------------------------------------
// Server
// ---------------------------------------------------------------------------

type server struct {
	log *slog.Logger

	mu sync.RWMutex
	// byResource holds executions newest-first.
	byResource map[string][]mapper.EDSExecution
	byID       map[string]*mapper.EDSExecution

	chaosMu     sync.RWMutex
	baseLatency time.Duration
	jitter      time.Duration
	failureRate float64
	unavailable bool
	malformed   bool
	schemaVer   string
}

func newServer(log *slog.Logger, seed int, baseLatency time.Duration) *server {
	s := &server{
		log:         log,
		byResource:  make(map[string][]mapper.EDSExecution),
		byID:        make(map[string]*mapper.EDSExecution),
		baseLatency: baseLatency,
		jitter:      baseLatency / 2,
		schemaVer:   "eds.v1",
	}
	s.seed(seed)
	return s
}

// edsStates is the source's own vocabulary. None of these tokens match the
// canonical ones, which is the point: the mapper has to do real work.
var edsStates = []string{"SUCCEEDED", "IN_PROGRESS", "ERRORED", "PENDING", "ABORTED"}

var edsOperations = []string{"ACTIVATE", "SUSPEND", "RESIZE", "MIGRATE", "RECONFIGURE"}

var edsResultingStates = []string{"LIVE", "PAUSED", "IMPAIRED", "BUILDING", "FAILED"}

func (s *server) seed(n int) {
	now := time.Now()
	for i := 1; i <= n; i++ {
		resourceID := fmt.Sprintf("R%03d", i)
		tenant := tenantFor(i)
		// Between one and four executions per resource, newest first.
		count := (i % 4) + 1
		var list []mapper.EDSExecution
		for j := 0; j < count; j++ {
			state := edsStates[(i+j)%len(edsStates)]
			started := now.Add(-time.Duration((j+1)*17) * time.Minute)
			completed := started.Add(90 * time.Second)
			rec := mapper.EDSExecution{
				ExecutionID:            fmt.Sprintf("E%03d-%d", i, j),
				TenantID:               tenant,
				ResourceID:             resourceID,
				CustomerID:             fmt.Sprintf("C%03d", (i%9)+1),
				Operation:              edsOperations[(i+j)%len(edsOperations)],
				State:                  state,
				ResultingResourceState: edsResultingStates[(i+j)%len(edsResultingStates)],
				StartedAt:              started.Format(time.RFC3339Nano),
				ExecutedAt:             started.Format(time.RFC3339Nano),
				UpdatedAt:              completed.Format(time.RFC3339Nano),
				SchemaVersion:          "eds.v1",
				WFSteps: []mapper.EDSStep{
					{StepID: "validate", Label: "Validate request", Ordinal: 1, State: "SUCCEEDED",
						StartedAt: started.Format(time.RFC3339Nano), CompletedAt: started.Add(5 * time.Second).Format(time.RFC3339Nano), Attempt: 1},
					{StepID: "apply", Label: "Apply change", Ordinal: 2, State: state,
						StartedAt: started.Add(5 * time.Second).Format(time.RFC3339Nano), Attempt: 1},
					{StepID: "verify", Label: "Verify outcome", Ordinal: 3, State: state,
						StartedAt: started.Add(60 * time.Second).Format(time.RFC3339Nano), Attempt: 1},
				},
				Actions: []mapper.EDSAction{
					{ActionID: "a1", Kind: "api.call", TargetRef: resourceID, Result: "ok",
						ExecutedAt: started.Add(10 * time.Second).Format(time.RFC3339Nano), ExecutedBy: "workflow-engine"},
				},
				Audit: []mapper.EDSAudit{
					{Timestamp: started.Format(time.RFC3339Nano), Actor: "workflow-engine", Event: "execution.started"},
					{Timestamp: completed.Format(time.RFC3339Nano), Actor: "workflow-engine", Event: "execution." + strings.ToLower(state)},
				},
			}
			// Only terminal executions carry a completion time and an outcome;
			// an in-progress one deliberately does not, so the BFF's handling
			// of a running execution is exercised by the seed data.
			if state != "IN_PROGRESS" && state != "PENDING" {
				rec.CompletedAt = completed.Format(time.RFC3339Nano)
				rec.Outcome = &mapper.EDSOutcome{
					OutcomeCode: state,
					Narrative:   fmt.Sprintf("%s completed with state %s", rec.Operation, state),
					Attributes:  map[string]string{"durationSeconds": "90"},
				}
			}
			if state == "ERRORED" {
				rec.Failure = &mapper.EDSFailure{
					FailureCode: "DOWNSTREAM_REJECTED",
					Detail:      "the target system rejected the change",
					Transient:   false,
					AtStep:      "apply",
				}
			}
			list = append(list, rec)
		}
		sort.SliceStable(list, func(a, b int) bool { return list[a].UpdatedAt > list[b].UpdatedAt })
		s.byResource[resourceID] = list
		for k := range list {
			s.byID[list[k].ExecutionID] = &s.byResource[resourceID][k]
		}
	}
}

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
// Routes
// ---------------------------------------------------------------------------

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /eds/v1/health", func(w http.ResponseWriter, _ *http.Request) {
		s.chaosMu.RLock()
		unavailable, rate := s.unavailable, s.failureRate
		s.chaosMu.RUnlock()
		switch {
		case unavailable:
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		case rate > 0:
			writeJSON(w, http.StatusOK, map[string]string{"status": "degraded"})
		default:
			writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		}
	})

	mux.HandleFunc("GET /eds/v1/executions", func(w http.ResponseWriter, r *http.Request) {
		if !s.admit(w, r) {
			return
		}
		tenant := r.URL.Query().Get("tenantId")
		resourceID := r.URL.Query().Get("resourceId")
		if resourceID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "resourceId is required"})
			return
		}
		limit := 25
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				limit = n
			}
		}
		offset := 0
		if c := r.URL.Query().Get("cursor"); c != "" {
			if n, err := strconv.Atoi(c); err == nil && n >= 0 {
				offset = n
			}
		}
		includeAudit := r.URL.Query().Get("includeAudit") == "true"

		s.mu.RLock()
		all := s.byResource[resourceID]
		s.mu.RUnlock()

		items := make([]mapper.EDSExecution, 0, limit)
		for _, e := range all {
			if tenant != "" && e.TenantID != tenant {
				continue
			}
			items = append(items, s.render(e, includeAudit))
		}
		total := len(items)
		if offset > total {
			offset = total
		}
		end := min(offset+limit, total)
		page := mapper.EDSExecutionPage{Items: items[offset:end], TotalCount: total}
		if end < total {
			page.NextCursor = strconv.Itoa(end)
		}
		writeJSON(w, http.StatusOK, page)
	})

	mux.HandleFunc("GET /eds/v1/executions/{executionId}", func(w http.ResponseWriter, r *http.Request) {
		if !s.admit(w, r) {
			return
		}
		tenant := r.URL.Query().Get("tenantId")
		includeAudit := r.URL.Query().Get("includeAudit") == "true"

		s.mu.RLock()
		rec, ok := s.byID[r.PathValue("executionId")]
		s.mu.RUnlock()
		if !ok || (tenant != "" && rec.TenantID != tenant) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "execution not found"})
			return
		}
		writeJSON(w, http.StatusOK, s.render(*rec, includeAudit))
	})

	mux.HandleFunc("GET /eds/v1/resources/{resourceId}/latest-execution", func(w http.ResponseWriter, r *http.Request) {
		if !s.admit(w, r) {
			return
		}
		tenant := r.URL.Query().Get("tenantId")
		s.mu.RLock()
		list := s.byResource[r.PathValue("resourceId")]
		s.mu.RUnlock()
		for _, e := range list {
			if tenant != "" && e.TenantID != tenant {
				continue
			}
			writeJSON(w, http.StatusOK, s.render(e, false))
			return
		}
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no executions for this resource"})
	})

	return mux
}

// render applies the schema-version override and strips audit records the
// caller did not ask for. Audit data is only ever returned on request, which
// is how the BFF's RBAC gate on it stays meaningful end to end.
func (s *server) render(e mapper.EDSExecution, includeAudit bool) mapper.EDSExecution {
	s.chaosMu.RLock()
	e.SchemaVersion = s.schemaVer
	malformed := s.malformed
	s.chaosMu.RUnlock()

	if !includeAudit {
		e.Audit = nil
	}
	if malformed {
		// Drop the identifier, which the BFF's mapper must reject as an
		// invalid record rather than pass through (REQ-EDGE-020).
		e.ExecutionID = ""
	}
	return e
}

// admit applies the injected latency and failures, and reports whether the
// request should be served.
func (s *server) admit(w http.ResponseWriter, r *http.Request) bool {
	s.chaosMu.RLock()
	base, jitter, rate, unavailable := s.baseLatency, s.jitter, s.failureRate, s.unavailable
	s.chaosMu.RUnlock()

	if unavailable {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "execution source is unavailable"})
		return false
	}

	d := base
	if jitter > 0 {
		d += time.Duration(rand.Int64N(int64(jitter)))
	}
	if d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-r.Context().Done():
			// The client gave up; do not write, the connection is going away.
			return false
		case <-t.C:
		}
	}

	if rate > 0 && rand.Float64() < rate {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "transient execution source failure"})
		return false
	}
	return true
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
		s.chaosMu.RLock()
		defer s.chaosMu.RUnlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"base_latency_ms": s.baseLatency.Milliseconds(),
			"jitter_ms":       s.jitter.Milliseconds(),
			"failure_rate":    s.failureRate,
			"unavailable":     s.unavailable,
			"malformed":       s.malformed,
			"schema_version":  s.schemaVer,
		})
	})

	mux.HandleFunc("PUT /chaos", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			BaseLatencyMS *int64   `json:"base_latency_ms"`
			JitterMS      *int64   `json:"jitter_ms"`
			FailureRate   *float64 `json:"failure_rate"`
			Unavailable   *bool    `json:"unavailable"`
			Malformed     *bool    `json:"malformed"`
			SchemaVersion *string  `json:"schema_version"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		s.chaosMu.Lock()
		if body.BaseLatencyMS != nil {
			s.baseLatency = time.Duration(*body.BaseLatencyMS) * time.Millisecond
		}
		if body.JitterMS != nil {
			s.jitter = time.Duration(*body.JitterMS) * time.Millisecond
		}
		if body.FailureRate != nil {
			s.failureRate = *body.FailureRate
		}
		if body.Unavailable != nil {
			s.unavailable = *body.Unavailable
		}
		if body.Malformed != nil {
			s.malformed = *body.Malformed
		}
		if body.SchemaVersion != nil {
			s.schemaVer = *body.SchemaVersion
		}
		s.chaosMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
	})

	mux.HandleFunc("DELETE /chaos", func(w http.ResponseWriter, _ *http.Request) {
		s.chaosMu.Lock()
		s.baseLatency, s.jitter = 120*time.Millisecond, 60*time.Millisecond
		s.failureRate, s.unavailable, s.malformed = 0, false, false
		s.schemaVer = "eds.v1"
		s.chaosMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]string{"status": "reset"})
	})

	// Start a new in-progress execution against a resource. This is how a test
	// puts the system into the state where the precedence policy's
	// execution-overrides-operational rule fires.
	mux.HandleFunc("POST /resources/{resourceId}/executions", func(w http.ResponseWriter, r *http.Request) {
		resourceID := r.PathValue("resourceId")
		q := r.URL.Query()
		operation := q.Get("operation")
		if operation == "" {
			operation = "RECONFIGURE"
		}
		state := q.Get("state")
		if state == "" {
			state = "IN_PROGRESS"
		}
		resulting := q.Get("resultingState")
		if resulting == "" {
			resulting = "BUILDING"
		}
		tenant := q.Get("tenantId")
		if tenant == "" {
			tenant = "local"
		}

		now := time.Now()
		rec := mapper.EDSExecution{
			ExecutionID:            fmt.Sprintf("E-%d", now.UnixNano()),
			TenantID:               tenant,
			ResourceID:             resourceID,
			Operation:              operation,
			State:                  state,
			ResultingResourceState: resulting,
			StartedAt:              now.Format(time.RFC3339Nano),
			UpdatedAt:              now.Format(time.RFC3339Nano),
			SchemaVersion:          "eds.v1",
		}
		s.mu.Lock()
		// Prepending allocates a new backing array, so every pointer already in
		// byID refers to the old one. Reindex the whole resource rather than
		// adding one entry, or the two maps quietly become views of different
		// arrays.
		s.byResource[resourceID] = append([]mapper.EDSExecution{rec}, s.byResource[resourceID]...)
		for k := range s.byResource[resourceID] {
			s.byID[s.byResource[resourceID][k].ExecutionID] = &s.byResource[resourceID][k]
		}
		s.mu.Unlock()

		writeJSON(w, http.StatusCreated, rec)
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
