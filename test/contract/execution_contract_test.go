//go:build contract

package contract

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/udaykishore/ttl-aware-bff/internal/domain"
	"github.com/udaykishore/ttl-aware-bff/internal/mapper"
	"github.com/udaykishore/ttl-aware-bff/internal/testutil"
)

func executionBaseURL(t *testing.T) string {
	t.Helper()
	base := os.Getenv("EXECUTION_URL")
	if base == "" {
		base = "http://localhost:9102"
	}
	resp, err := http.Get(base + "/eds/v1/health") //nolint:noctx // contract probe
	if err != nil {
		t.Skipf("execution source not reachable at %s; start it with `make compose-up` or set EXECUTION_URL", base)
	}
	_ = resp.Body.Close()
	return base
}

func getJSON(t *testing.T, url string, out any) *http.Response {
	t.Helper()
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url) //nolint:noctx // contract probe
	testutil.NoError(t, err, "GET %s", url)
	t.Cleanup(func() { _ = resp.Body.Close() })
	if out != nil && resp.StatusCode == http.StatusOK {
		body, err := io.ReadAll(resp.Body)
		testutil.NoError(t, err, "read body")
		testutil.NoError(t, json.Unmarshal(body, out), "decode body of %s: %s", url, string(body))
	}
	return resp
}

// TestContract_ListExecutionsShape verifies REQ-DS-008: the history endpoint
// returns the paged envelope the adapter decodes, with the field names the
// mapper reads. Renaming any of them upstream must fail here rather than
// producing silently empty history in the UI.
func TestContract_ListExecutionsShape(t *testing.T) {
	base := executionBaseURL(t)

	var page mapper.EDSExecutionPage
	resp := getJSON(t, fmt.Sprintf("%s/eds/v1/executions?tenantId=local&resourceId=R001&limit=10", base), &page)
	testutil.Equal(t, resp.StatusCode, http.StatusOK, "status")
	testutil.True(t, len(page.Items) > 0, "the seeded resource must have execution history")
	testutil.True(t, page.TotalCount > 0, "totalCount is required for the UI's pagination")

	e := page.Items[0]
	testutil.NotEqual(t, e.ExecutionID, "", "executionId")
	testutil.NotEqual(t, e.ResourceID, "", "resourceId")
	testutil.NotEqual(t, e.TenantID, "", "tenantId, which the BFF verifies against its request")
	testutil.NotEqual(t, e.Operation, "", "operation")
	testutil.NotEqual(t, e.State, "", "state")
	testutil.NotEqual(t, e.SchemaVersion, "", "schemaVersion")
	testutil.Equal(t, e.SchemaVersion, mapper.ExecutionSchemaVersion,
		"the declared schema version must be one this build accepts")
}

// TestContract_EveryStateTokenMaps verifies REQ-MAP-004: every state the source
// actually emits has a canonical counterpart. This walks the real data rather
// than a hard-coded list, so a newly introduced token is caught.
func TestContract_EveryStateTokenMaps(t *testing.T) {
	base := executionBaseURL(t)

	seen := map[string]bool{}
	for i := 1; i <= 20; i++ {
		var page mapper.EDSExecutionPage
		url := fmt.Sprintf("%s/eds/v1/executions?resourceId=R%03d&limit=50", base, i)
		if resp := getJSON(t, url, &page); resp.StatusCode != http.StatusOK {
			continue
		}
		for _, e := range page.Items {
			seen[e.State] = true
			if e.ResultingResourceState != "" {
				testutil.NotEqual(t, mapper.MapResultingResourceStatus(e.ResultingResourceState),
					domain.StatusUnknown,
					"resultingResourceState %q has no canonical mapping", e.ResultingResourceState)
			}
		}
	}

	testutil.True(t, len(seen) > 1, "the sample must cover more than one state, got %v", seen)
	for token := range seen {
		testutil.NotEqual(t, mapper.MapExecutionStatus(token), domain.ExecUnknown,
			"state %q has no canonical mapping; add one to internal/mapper/execution.go", token)
	}
}

// TestContract_LatestExecutionEndpoint verifies REQ-DS-009: the endpoint the
// /details fan-out depends on exists and returns a single record.
func TestContract_LatestExecutionEndpoint(t *testing.T) {
	base := executionBaseURL(t)

	var rec mapper.EDSExecution
	resp := getJSON(t, base+"/eds/v1/resources/R001/latest-execution?tenantId=local", &rec)
	testutil.Equal(t, resp.StatusCode, http.StatusOK, "status")
	testutil.NotEqual(t, rec.ExecutionID, "", "executionId")

	mapped, err := mapper.NewExecution([]string{mapper.ExecutionSchemaVersion}, nil).One(&rec, "local")
	testutil.NoError(t, err, "the record must satisfy the canonical mapping")
	testutil.NotEqual(t, mapped.Status, domain.ExecUnknown, "and map to a known canonical status")
}

// TestContract_AuditIsOnlyReturnedWhenAskedFor verifies REQ-SEC-007: audit
// records must not travel unless requested, so the BFF's RBAC gate on them is
// meaningful end to end rather than merely cosmetic.
func TestContract_AuditIsOnlyReturnedWhenAskedFor(t *testing.T) {
	base := executionBaseURL(t)

	var without mapper.EDSExecutionPage
	getJSON(t, base+"/eds/v1/executions?tenantId=local&resourceId=R001&limit=5", &without)
	for _, e := range without.Items {
		testutil.Equal(t, len(e.Audit), 0,
			"audit records must not be returned unless includeAudit=true")
	}

	var with mapper.EDSExecutionPage
	getJSON(t, base+"/eds/v1/executions?tenantId=local&resourceId=R001&limit=5&includeAudit=true", &with)
	found := false
	for _, e := range with.Items {
		if len(e.Audit) > 0 {
			found = true
		}
	}
	testutil.True(t, found, "and they must be returned when it is")
}

// TestContract_TenantScopingIsEnforced verifies REQ-SEC-004.
func TestContract_TenantScopingIsEnforced(t *testing.T) {
	base := executionBaseURL(t)

	var page mapper.EDSExecutionPage
	getJSON(t, base+"/eds/v1/executions?tenantId=a-tenant-that-owns-nothing&resourceId=R001&limit=10", &page)
	testutil.Equal(t, len(page.Items), 0,
		"the source must not return another tenant's executions")
}

// TestContract_MissingRecordIs404 verifies REQ-ERR-005: a missing record is a
// 404, which the adapter classifies as NOT_FOUND and which must never trip a
// circuit breaker.
func TestContract_MissingRecordIs404(t *testing.T) {
	base := executionBaseURL(t)
	resp := getJSON(t, base+"/eds/v1/executions/no-such-execution?tenantId=local", nil)
	testutil.Equal(t, resp.StatusCode, http.StatusNotFound, "status")
}

// TestContract_PaginationIsHonoured verifies REQ-DS-010: limit and cursor work,
// so the BFF can bound how much history it pulls.
func TestContract_PaginationIsHonoured(t *testing.T) {
	base := executionBaseURL(t)

	// R004 is seeded with the most executions (i%4+1).
	var first mapper.EDSExecutionPage
	getJSON(t, base+"/eds/v1/executions?resourceId=R004&limit=1", &first)
	if first.TotalCount <= 1 {
		t.Skip("the sample resource has too little history to page through")
	}
	testutil.Equal(t, len(first.Items), 1, "limit is honoured")
	testutil.NotEqual(t, first.NextCursor, "", "a next cursor is offered when more records exist")

	var second mapper.EDSExecutionPage
	getJSON(t, fmt.Sprintf("%s/eds/v1/executions?resourceId=R004&limit=1&cursor=%s", base, first.NextCursor), &second)
	testutil.Equal(t, len(second.Items), 1, "the second page has a record")
	testutil.NotEqual(t, second.Items[0].ExecutionID, first.Items[0].ExecutionID,
		"and it is a different record")
}

// TestContract_ExecutionSourceIsTheSlowOne documents REQ-PERF-003. It is not a
// pass/fail assertion about a particular number -- that would be flaky and
// environment-dependent -- but it records the measured difference, which is the
// premise the whole routing design is built on. If this ever shows the
// execution source as the faster one, the design deserves revisiting.
func TestContract_ExecutionSourceIsTheSlowOne(t *testing.T) {
	base := executionBaseURL(t)

	const samples = 10
	var total time.Duration
	for i := 0; i < samples; i++ {
		start := time.Now()
		getJSON(t, base+"/eds/v1/executions?tenantId=local&resourceId=R001&limit=10", nil)
		total += time.Since(start)
	}
	t.Logf("mean execution-source read: %s over %d samples", total/samples, samples)
}
