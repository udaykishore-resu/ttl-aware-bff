// Package mapper translates each source's native schema into the canonical
// domain model. It is the only place where a source field name appears next to
// a canonical field name.
//
// Two rules govern everything here:
//
//  1. Mapping is total and explicit. Every source enum value has a named
//     canonical counterpart, and an unrecognised value maps to the canonical
//     UNKNOWN rather than being passed through. A source that adds an enum
//     member must never leak that token to the UI.
//  2. Mapping never invents data. A field the source did not populate stays
//     empty; it is the aggregator's and the precedence policy's job to decide
//     what to do about that, not the mapper's.
//
// Traceability: REQ-MAP-001..REQ-MAP-009, REQ-EDGE-017, REQ-EDGE-020.
package mapper

import (
	"fmt"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	opsv1 "github.com/udaykishore-resu/ttl-aware-bff/internal/datasource/operational/opsv1"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/domain"
	"github.com/udaykishore-resu/ttl-aware-bff/pkg/errs"
)

// OperationalSchemaVersion is the ODS contract version this build understands.
const OperationalSchemaVersion = "ods.v1"

// operationalStatus is the complete, explicit enum mapping. A missing entry is
// a compile-time-visible gap rather than a silent pass-through.
var operationalStatus = map[opsv1.ResourceState]domain.ResourceStatus{
	opsv1.ResourceState_RESOURCE_STATE_UNSPECIFIED:  domain.StatusUnknown,
	opsv1.ResourceState_RESOURCE_STATE_PROVISIONING: domain.StatusPending,
	opsv1.ResourceState_RESOURCE_STATE_ACTIVE:       domain.StatusActive,
	opsv1.ResourceState_RESOURCE_STATE_SUSPENDED:    domain.StatusSuspended,
	opsv1.ResourceState_RESOURCE_STATE_DEGRADED:     domain.StatusDegraded,
	opsv1.ResourceState_RESOURCE_STATE_TERMINATING:  domain.StatusTerminating,
	opsv1.ResourceState_RESOURCE_STATE_TERMINATED:   domain.StatusTerminated,
	opsv1.ResourceState_RESOURCE_STATE_ERROR:        domain.StatusError,
}

// MapOperationalStatus converts a source state to the canonical vocabulary.
// Exported so contract tests can assert the mapping is total.
func MapOperationalStatus(s opsv1.ResourceState) domain.ResourceStatus {
	if v, ok := operationalStatus[s]; ok {
		return v
	}
	return domain.StatusUnknown
}

// Operational maps ODS records into canonical resources.
type Operational struct {
	// accepted is the set of schema versions this build will consume. Empty
	// means accept anything, which is only appropriate in development.
	accepted map[string]struct{}
	// onSchemaMismatch is called when a record declares an unknown version.
	onSchemaMismatch func(got string)
}

// NewOperational builds the mapper.
func NewOperational(acceptedVersions []string, onSchemaMismatch func(string)) *Operational {
	m := &Operational{onSchemaMismatch: onSchemaMismatch}
	if len(acceptedVersions) > 0 {
		m.accepted = make(map[string]struct{}, len(acceptedVersions))
		for _, v := range acceptedVersions {
			m.accepted[v] = struct{}{}
		}
	}
	if m.onSchemaMismatch == nil {
		m.onSchemaMismatch = func(string) {}
	}
	return m
}

// Resource maps a full operational record.
//
// tenantID is passed in rather than trusted from the payload: the BFF asked
// for one tenant's data and must verify it got it. A record carrying a
// different tenant is a hard failure, never a silently accepted response
// (REQ-MT-004, REQ-EDGE-016).
func (m *Operational) Resource(src *opsv1.OperationalResource, tenantID string) (*domain.Resource, domain.Freshness, error) {
	if src == nil {
		return nil, domain.Freshness{}, errs.New(errs.CodeUpstreamInvalidPayload, "operational source returned an empty record").
			WithSource(string(domain.SourceOperational))
	}
	if err := m.checkSchema(src.GetSchemaVersion()); err != nil {
		return nil, domain.Freshness{}, err
	}
	if src.GetTenantId() != "" && tenantID != "" && src.GetTenantId() != tenantID {
		return nil, domain.Freshness{}, errs.New(errs.CodeTenantMismatch,
			"operational source returned a record belonging to another tenant").
			WithSource(string(domain.SourceOperational)).
			WithOp("mapper.operational.resource")
	}
	if src.GetResourceId() == "" {
		return nil, domain.Freshness{}, errs.New(errs.CodeUpstreamInvalidPayload,
			"operational record is missing its resource identifier").
			WithSource(string(domain.SourceOperational))
	}

	out := &domain.Resource{
		TenantID:            firstNonEmpty(src.GetTenantId(), tenantID),
		ResourceID:          src.GetResourceId(),
		CustomerID:          src.GetCustomerRef(), // customer_ref -> customerId
		Type:                src.GetResourceType(),
		Status:              MapOperationalStatus(src.GetState()),
		SubState:            src.GetSubstate(),
		Configuration:       copyMap(src.GetConfiguration()),
		Labels:              copyMap(src.GetLabels()),
		Metadata:            copyMap(src.GetOperationalMetadata()),
		InFlightExecutionID: src.GetInFlightExecutionRef(),
		ObservedAt:          ts(src.GetFreshness().GetLastUpdated()),
		SchemaVersion:       src.GetSchemaVersion(),
	}

	if o := src.GetOwnership(); o != nil && (o.GetOwnerId() != "" || o.GetOwnerEmail() != "") {
		out.Owner = &domain.Owner{
			ID:         o.GetOwnerId(),
			Type:       o.GetOwnerType(),
			Email:      o.GetOwnerEmail(),
			CostCentre: o.GetCostCentre(),
		}
	}
	if ms := src.GetMetrics(); len(ms) > 0 {
		out.Metrics = make([]domain.Metric, 0, len(ms))
		for _, s := range ms {
			if s == nil || s.GetName() == "" {
				continue
			}
			out.Metrics = append(out.Metrics, domain.Metric{
				Name:      s.GetName(),
				Value:     s.GetValue(),
				Unit:      s.GetUnit(),
				SampledAt: ts(s.GetSampledAt()),
			})
		}
	}
	if t := src.GetTopology(); t != nil && (t.GetRegion() != "" || t.GetCluster() != "" || len(t.GetUpstreamRefs()) > 0) {
		out.Topology = &domain.Topology{
			Region:     t.GetRegion(),
			Zone:       t.GetZone(),
			Cluster:    t.GetCluster(),
			Upstream:   copySlice(t.GetUpstreamRefs()),
			Downstream: copySlice(t.GetDownstreamRefs()),
		}
	}

	return out, m.freshness(src.GetFreshness()), nil
}

// State maps the narrow status response.
func (m *Operational) State(resp *opsv1.GetResourceStateResponse, tenantID string) (*domain.Resource, domain.Freshness, error) {
	if resp == nil || resp.GetResourceId() == "" {
		return nil, domain.Freshness{}, errs.New(errs.CodeUpstreamInvalidPayload,
			"operational source returned an empty state record").
			WithSource(string(domain.SourceOperational))
	}
	out := &domain.Resource{
		TenantID:            tenantID,
		ResourceID:          resp.GetResourceId(),
		Status:              MapOperationalStatus(resp.GetState()),
		SubState:            resp.GetSubstate(),
		InFlightExecutionID: resp.GetInFlightExecutionRef(),
		ObservedAt:          ts(resp.GetFreshness().GetLastUpdated()),
	}
	return out, m.freshness(resp.GetFreshness()), nil
}

// Freshness converts a source freshness envelope into the canonical form
// WITHOUT evaluating it. Evaluation against a TTL is the freshness manager's
// job; the mapper only translates.
func (m *Operational) freshness(f *opsv1.FreshnessEnvelope) domain.Freshness {
	if f == nil {
		return domain.Freshness{State: domain.FreshnessUnknown, Source: domain.SourceOperational}
	}
	return domain.Freshness{
		State:      domain.FreshnessUnknown,
		ObservedAt: ts(f.GetLastUpdated()),
		Source:     domain.SourceOperational,
		Version:    f.GetVersion(),
	}
}

func (m *Operational) checkSchema(got string) error {
	if m.accepted == nil {
		return nil
	}
	if got == "" {
		// A source that declares nothing is treated as the version this build
		// was written against; refusing it would break on the first deploy of
		// a source that has not adopted the field yet.
		return nil
	}
	if _, ok := m.accepted[got]; ok {
		return nil
	}
	m.onSchemaMismatch(got)
	return errs.New(errs.CodeSchemaVersionMismatch,
		fmt.Sprintf("operational source declared unsupported schema version %q", got)).
		WithSource(string(domain.SourceOperational)).
		WithOp("mapper.operational.schema")
}

// ---------------------------------------------------------------------------
// shared helpers
// ---------------------------------------------------------------------------

// ts converts a protobuf timestamp to a Go time.
//
// An explicitly-present but zero Timestamp (seconds 0, nanos 0) is treated as
// ABSENT rather than as 1970-01-01. A source that sends an unset field inside a
// populated message is saying "I do not know when this was refreshed", and
// mapping that to the epoch would make the freshness manager compute an age of
// half a century and report the record as catastrophically stale instead of
// reporting UNKNOWN (REQ-MAP-010, REQ-TTL-006).
func ts(p *timestamppb.Timestamp) time.Time {
	if p == nil || !p.IsValid() {
		return time.Time{}
	}
	if p.GetSeconds() == 0 && p.GetNanos() == 0 {
		return time.Time{}
	}
	return p.AsTime()
}

func copyMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func copySlice(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
