// Package policy holds the two decisions that must never be implicit: which
// source *can* supply a field, and which source *wins* when both do.
//
// Keeping them here, driven by configuration, is what stops source preference
// from being an accident of the order in which someone wrote an if statement.
//
// Traceability: REQ-PREC-001..REQ-PREC-008, REQ-RT-004.
package policy

import (
	"sort"
	"strings"

	"github.com/udaykishore-resu/ttl-aware-bff/internal/config"
	"github.com/udaykishore-resu/ttl-aware-bff/internal/domain"
)

// Canonical field names. These are the strings used in configuration, in
// provenance, and in the router's required-field logic. They are declared as
// constants so a typo in one place cannot silently disable a precedence rule.
const (
	FieldStatus           = "status"
	FieldSubState         = "subState"
	FieldConfiguration    = "configuration"
	FieldMetrics          = "metrics"
	FieldTopology         = "topology"
	FieldOwner            = "owner"
	FieldLabels           = "labels"
	FieldCustomerID       = "customerId"
	FieldType             = "type"
	FieldLatestExecution  = "latestExecution"
	FieldExecutionHistory = "executionHistory"
	FieldExecutionStatus  = "executionStatus"
	FieldLastOperation    = "lastOperation"
	FieldAudit            = "audit"
	FieldWorkflowSteps    = "workflowSteps"
)

// FieldCatalog answers "which sources can supply this field?" It is derived
// from the precedence configuration, so the two can never disagree: a field
// with no configured precedence has no supplier and is rejected at config
// validation time.
type FieldCatalog struct {
	suppliers map[string][]domain.SourceKind
}

// NewFieldCatalog builds the catalogue from precedence configuration.
func NewFieldCatalog(cfg config.PrecedenceConfig) *FieldCatalog {
	c := &FieldCatalog{suppliers: make(map[string][]domain.SourceKind, len(cfg.Fields))}
	for field, names := range cfg.Fields {
		kinds := make([]domain.SourceKind, 0, len(names))
		for _, n := range names {
			if k, ok := domain.ParseSourceKind(n); ok && !k.IsNone() {
				kinds = append(kinds, k)
			}
		}
		c.suppliers[field] = kinds
	}
	return c
}

// Suppliers returns the sources that can supply a field, most authoritative
// first. An unknown field returns nil, which the router treats as "no
// constraint" rather than as an error: a caller asking for a field the BFF
// does not model is a request-validation problem, not a routing one.
func (c *FieldCatalog) Suppliers(field string) []domain.SourceKind {
	return c.suppliers[field]
}

// SourcesFor returns the set of sources needed to satisfy every requested
// field, using each field's most authoritative supplier.
func (c *FieldCatalog) SourcesFor(fields []string) map[domain.SourceKind]bool {
	out := map[domain.SourceKind]bool{}
	for _, f := range fields {
		s := c.suppliers[f]
		if len(s) == 0 {
			continue
		}
		out[s[0]] = true
	}
	return out
}

// ExclusiveTo reports whether every listed field can only come from one
// source, and which source that is. This is what lets the router short-circuit
// to a single source for a request that only asks for execution history.
func (c *FieldCatalog) ExclusiveTo(fields []string) (domain.SourceKind, bool) {
	var only domain.SourceKind
	seen := false
	for _, f := range fields {
		s := c.suppliers[f]
		if len(s) == 0 {
			continue
		}
		if len(s) > 1 {
			return domain.SourceNone, false
		}
		if !seen {
			only, seen = s[0], true
			continue
		}
		if s[0] != only {
			return domain.SourceNone, false
		}
	}
	if !seen {
		return domain.SourceNone, false
	}
	return only, true
}

// KnownFields lists every field the catalogue models, sorted. Used by the
// request validator to reject unknown field selections with a helpful message.
func (c *FieldCatalog) KnownFields() []string {
	out := make([]string, 0, len(c.suppliers))
	for f := range c.suppliers {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// DefaultFieldsFor returns the canonical fields a request type is expected to
// produce when the caller does not name any. Keeping this mapping in the
// policy package (rather than in the handler) means the router and the
// aggregator agree about what a request needs.
func DefaultFieldsFor(requestType string) []string {
	switch strings.ToLower(requestType) {
	case "resource_status":
		// Status alone. The sub-state is decoration -- it is omitempty on the
		// wire and no caller can depend on it -- so listing it as required
		// would make every answer from a source that does not model it look
		// incomplete.
		return []string{FieldStatus}
	case "resource_configuration":
		return []string{FieldConfiguration}
	case "resource_read":
		return []string{FieldStatus, FieldSubState, FieldConfiguration, FieldOwner, FieldMetrics, FieldTopology, FieldLabels, FieldCustomerID}
	case "execution_status":
		return []string{FieldExecutionStatus, FieldWorkflowSteps}
	case "execution_history":
		return []string{FieldExecutionHistory}
	case "resource_details":
		return []string{
			FieldStatus, FieldSubState, FieldConfiguration, FieldOwner, FieldMetrics, FieldTopology,
			FieldLatestExecution, FieldExecutionHistory,
		}
	default:
		return nil
	}
}
