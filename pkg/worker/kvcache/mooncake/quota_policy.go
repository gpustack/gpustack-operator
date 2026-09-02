package mooncake

import (
	"fmt"
	"strings"
	"unicode"

	yaml "gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"gpustack.ai/gpustack/pkg/utils/quantityx"
)

// quotaPolicyVersion is emitted unconditionally, and it is the only value this file's schema knows.
//
// A policy document without it does not fail to load — the leader throws std::runtime_error out of
// its loader and aborts, which arrives as CrashLoopBackOff on a Pod that was healthy a moment ago.
// So it is a constant rendered by the writer, never a field an input can leave out.
const quotaPolicyVersion = 1

// QuotaPolicyTenant is one entry of the tenant quota policy: a reuse domain and the byte ceiling
// requested for it.
//
// The quota is a resource.Quantity rather than a count, because that is the form the API carries it
// in; the renderer is the one place it becomes bytes, and quantityx.OverflowsInt64 guards that
// conversion.
type QuotaPolicyTenant struct {
	Name  string
	Quota resource.Quantity
}

// quotaPolicyDocument mirrors the file the leader loads. Field order here IS the emitted order, which
// is what makes one render diff cleanly against the last.
type quotaPolicyDocument struct {
	Version int                   `yaml:"version"`
	Tenants []quotaPolicyDocEntry `yaml:"tenants"`
}

type quotaPolicyDocEntry struct {
	Name string `yaml:"name"`
	// An int64 count of bytes, which is the form the leader itself rewrites the file into after any
	// admin-API change: a hand-written `quota: 1GB` comes back as `quota: 1073741824`. Writing that
	// form directly means a render and the leader's own rewrite do not fight over the file.
	Quota int64 `yaml:"quota"`
}

// RenderQuotaPolicy renders the whole tenant quota policy file from tenants.
//
// It validates first, through ValidateQuotaPolicyTenants, and a refused input yields an error and NO
// output: never a file with the offending entry dropped, because a silently shortened tenant list is
// a quota nobody set. An empty tenant set is not a refusal — it renders a valid file with an empty
// list, which is what a pool with no bindings legitimately means.
//
// The webhook calls the validator and this calls it too, so the two cannot form separate opinions
// about what is safe to hand the leader.
func RenderQuotaPolicy(tenants []QuotaPolicyTenant) ([]byte, error) {
	if errs := ValidateQuotaPolicyTenants(tenants, field.NewPath("tenants")); len(errs) > 0 {
		return nil, errs.ToAggregate()
	}

	doc := quotaPolicyDocument{
		Version: quotaPolicyVersion,
		Tenants: make([]quotaPolicyDocEntry, 0, len(tenants)),
	}
	for _, t := range tenants {
		doc.Tenants = append(doc.Tenants, quotaPolicyDocEntry{
			Name:  t.Name,
			Quota: t.Quota.Value(),
		})
	}

	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("marshalling the tenant quota policy: %w", err)
	}
	return out, nil
}

// ValidateQuotaPolicyTenants reports every input the leader would refuse, or crash on, as a typed
// field error under fldPath.
//
// It exists so admission can refuse the object instead of letting the reconciler write a file the
// leader aborts over. Each rule below is one the leader enforces itself, and reaching it there costs
// either a 400 nobody sees or a process that will not start:
//
//   - a name is non-empty, unique across the set, and does not begin with "_": the leader answers
//     `Missing or invalid tenant_id` to the first two, and reserves the underscore prefix.
//   - a name carries no NUL and no control character, neither of which survives the round trip
//     through the file the leader rewrites.
//   - a quota is strictly positive and survives the conversion into an int64 byte count: the leader
//     answers `Tenant quota must be positive`, and a quantity above the int64 maximum reads as not
//     positive without ever failing (see quantityx.OverflowsInt64).
//
// Every offending entry is reported, not just the first, so one apply tells an operator about the
// whole set rather than one round trip per bad name.
func ValidateQuotaPolicyTenants(tenants []QuotaPolicyTenant, fldPath *field.Path) field.ErrorList {
	var errs field.ErrorList

	seen := make(map[string]struct{}, len(tenants))
	for i, t := range tenants {
		namePath := fldPath.Index(i).Child("name")
		if nameErrs := ValidateQuotaPolicyTenantName(t.Name, namePath); len(nameErrs) > 0 {
			errs = append(errs, nameErrs...)
		} else if _, ok := seen[t.Name]; ok {
			// Uniqueness stays HERE rather than in the per-name rule, because it is a property of the
			// set and not of any one name. The SECOND claim is the one refused, never the first: one
			// ledger entry per name means a last-wins merge would hand the domain a quota its own
			// Binding never asked for.
			errs = append(errs, field.Duplicate(namePath, t.Name))
		} else {
			seen[t.Name] = struct{}{}
		}

		errs = append(errs, ValidateQuotaPolicyQuota(t.Quota, fldPath.Index(i).Child("quota"))...)
	}

	return errs
}

// ValidateQuotaPolicyTenantName reports why one name could not be a tenant in the policy file.
//
// It is the LOWER BOUND — what the leader itself refuses — and not a shape this project prefers. A
// caller may hold a name to something stricter; a reuse domain is held to a DNS-1123 label, which
// happens to satisfy every rule here. That makes this call look redundant today, and it is the point
// of calling it anyway: the leader's rules live in one place, so a rule added to them reaches every
// admitting path without anyone remembering to copy it.
func ValidateQuotaPolicyTenantName(name string, fldPath *field.Path) field.ErrorList {
	switch {
	case name == "":
		return field.ErrorList{field.Required(fldPath,
			"a reuse domain with no name is not a tenant the leader can hold quota for")}
	case strings.HasPrefix(name, "_"):
		return field.ErrorList{field.Invalid(fldPath, name,
			`must not start with "_": the leader reserves that prefix and answers `+
				`"Missing or invalid tenant_id"`)}
	// NUL is a control character like any other here, and is called out in the message only because
	// it is the one that truncates rather than garbles.
	case strings.ContainsFunc(name, unicode.IsControl):
		return field.ErrorList{field.Invalid(fldPath, name,
			"must not contain NUL or any other control character: it does not survive the "+
				"policy file the leader loads and rewrites")}
	}

	return nil
}

// ValidateQuotaPolicyQuota reports why one quantity could not be a quota in the policy file.
//
// It is exported because a byte ceiling is refused in more than one place — a Binding's own, and the
// pool total that bounds it — and each has to refuse for the same reasons. A second copy of these two
// rules would drift in the ADMITTING direction, which is the one nothing notices: an object gets in,
// and the fault appears later as a master that will not start.
func ValidateQuotaPolicyQuota(quota resource.Quantity, fldPath *field.Path) field.ErrorList {
	switch {
	// Asked BEFORE the sign, because a quantity above the int64 maximum reports itself as MinInt64
	// or as 0 — refusing it as "not positive" would name a number the input does not contain.
	case quantityx.OverflowsInt64(quota):
		return field.ErrorList{field.Invalid(fldPath, quota.String(),
			"must not exceed 9223372036854775807 (2^63-1) bytes: the policy file carries a "+
				"signed 64-bit count, and a larger one does not survive the conversion")}
	case quota.CmpInt64(0) <= 0:
		return field.ErrorList{field.Invalid(fldPath, quota.String(),
			`must be greater than 0: the leader answers "Tenant quota must be positive", and a `+
				"tenant with no quota is one that can write nothing")}
	}

	return nil
}
