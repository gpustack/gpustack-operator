package mooncake

import (
	core "k8s.io/api/core/v1"
	rbac "k8s.io/api/rbac/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
	"gpustack.ai/gpustack/pkg/worker/kvcache"
)

// MemberRBACObjectName is the name of the account a backend's members run as under HA.
//
// Per BACKEND, not per group: every group of one backend reads the same Lease, and the read is the
// only thing this account grants.
func MemberRBACObjectName(kvcb *workercore.KVCacheBackend) string {
	return kvcb.Name + "-member"
}

// leaderNeedsAPIAccess reports whether an election runs. The leader then needs API access, and the
// member account stays available for an explicit Lease address.
//
// The election exists only ABOVE ONE REPLICA. A single process has nothing to elect between, so
// highAvailability with one replica renders no election flag, no Lease and no API token -- and the
// leader runs exactly the command line it would run with the field unset. That reading is also the
// only one a store image built without the k8s-lease backend survives: the backend's absence fails
// the master at startup the moment the election flags appear, so rendering them for one replica
// would buy nothing and cost such an image the whole backend.
//
// The pairing is re-evaluated on every render, not decided at create: raising replicas past one
// turns the election on, and lowering back to one turns it off. Both flips restart the leader and
// roll every member because their token and account settings change. An explicit Lease address also
// changes shape with the election -- see MemberMasterEntry.
//
// Without an election neither role has a reason to hold a token. Under HA, the leader elects through
// a Lease and an explicitly Lease-addressed member reads it. The member account is rendered for both
// address values. The predicate is named because five renderings ask it.
func leaderNeedsAPIAccess(leader workercore.KVCacheBackendLeader) bool {
	if leader.HighAvailability == nil {
		return false
	}
	// Nil is the schema's default of one, which is below the election's floor the same as an
	// explicit 1.
	return leader.Replicas != nil && *leader.Replicas > 1
}

// leaderServiceAccountName is the account the leader Pod runs as, and it is EMPTY without HA.
//
// Empty is what the API server reads as "default", which is what the Pod ran as before this existed.
// Naming an account unconditionally would be the other obvious shape and is wrong: the account is
// only rendered under HA, so a Pod naming one outside HA would never get a token.
func leaderServiceAccountName(kvcb *workercore.KVCacheBackend) string {
	if !leaderNeedsAPIAccess(kvcb.Spec.Connection.Managed.Leader) {
		return ""
	}

	return LeaderObjectName(kvcb)
}

// memberServiceAccountName is the same for a member group. See leaderServiceAccountName.
func memberServiceAccountName(kvcb *workercore.KVCacheBackend) string {
	if !leaderNeedsAPIAccess(kvcb.Spec.Connection.Managed.Leader) {
		return ""
	}

	return MemberRBACObjectName(kvcb)
}

// HARBAC is the API access one role needs to take part in an election. Every field is nil, or none
// is.
//
// They travel together because a partial set is a failure this operator cannot observe: an account
// with no binding, or a binding with no role, authenticates as nobody, and neither the API server
// nor the store says so in a way that names the missing object.
type HARBAC struct {
	ServiceAccount *core.ServiceAccount
	Role           *rbac.Role
	RoleBinding    *rbac.RoleBinding
}

// Wanted reports whether this backend asks for the access at all.
func (r HARBAC) Wanted() bool { return r.ServiceAccount != nil }

// RenderLeaderRBAC renders what a leader needs to WIN an election, or nothing.
//
// REQUIRED: every verb here is read from what the store's C++ side CALLS, not from what its Go
// wrapper offers. The two differ, and the difference is a real over-grant: the wrapper exports
// K8sLeaseWatchHolder and issues a collection Watch, but nothing in the C++ tree calls it -- the
// k8s coordinator follows the view by re-reading it on a timer instead. A grant derived from the
// wrapper's surface therefore includes `watch`, which nothing uses.
func RenderLeaderRBAC(kvcb *workercore.KVCacheBackend) HARBAC {
	return renderHARBAC(kvcb, LeaderObjectName(kvcb), leaderResourceNoteRole, []rbac.PolicyRule{
		{
			APIGroups: []string{"coordination.k8s.io"},
			Resources: []string{"leases"},
			// The LeaseLock's three operations, and no more: it reads the record, creates it when
			// absent, and renews it.
			Verbs: []string{"create", "get", "update"},
		},
		{
			APIGroups: []string{""},
			Resources: []string{"pods"},
			// The leader labeling its OWN Pod once it wins, which the store does on its own
			// initiative. This operator neither asks for it nor reads it, and cannot decline it
			// either: the pod identity flags it has always rendered are two of the three conditions,
			// and turning HA on supplies the third. Withholding the verb does not switch the
			// behavior off -- it leaves a thread logging a warning every second for the life of the
			// leader.
			Verbs: []string{"patch"},
		},
	})
}

// RenderMemberRBAC renders the account an explicitly Lease-addressed member needs, or nothing.
//
// A Lease-addressed member reads the holder at connect time and re-reads it to follow an election.
// It never takes leadership, so this account is narrower than the leader's. The default Service
// address does not use the account, but both address values receive it.
//
// LIMITED: the read is a POLL. The k8s coordinator's WaitForViewChange re-reads the holder every
// 200ms rather than watching, so each Lease-addressed member issues about five reads per second
// against the API server, and that cost scales with the Lease-addressed member count.
func RenderMemberRBAC(kvcb *workercore.KVCacheBackend) HARBAC {
	return renderHARBAC(kvcb, MemberRBACObjectName(kvcb), memberResourceNoteRole, []rbac.PolicyRule{
		{
			APIGroups: []string{"coordination.k8s.io"},
			Resources: []string{"leases"},
			Verbs:     []string{"get"},
		},
	})
}

// renderHARBAC builds one role's three objects in the shared system namespace, owned by the backend.
//
// LIMITED: no resourceNames on the rules its callers pass. RBAC does not apply them to `create`, and
// narrowing only some verbs would read as isolating one backend's Lease from another's without doing
// it. What these grant is namespace-wide on leases, and every account here is in one trust domain in
// one namespace this operator owns.
func renderHARBAC(
	kvcb *workercore.KVCacheBackend, name, noteRole string, rules []rbac.PolicyRule,
) HARBAC {
	if !leaderNeedsAPIAccess(kvcb.Spec.Connection.Managed.Leader) {
		return HARBAC{}
	}

	// The identity labels every object a backend renders carries, with the role's own component
	// value. Built from the same constants as the workloads' rather than restated, because these
	// three objects are found the same way the Deployment and the DaemonSets are.
	labels := map[string]string{
		labelKeyName:      labelValueName,
		labelKeyInstance:  kvcb.Name,
		labelKeyComponent: noteRole,
		labelKeyPartOf:    labelValuePartOf,
	}
	objectMeta := func() meta.ObjectMeta {
		return meta.ObjectMeta{
			Name:      name,
			Namespace: kuberess.SystemNamespaceName,
			Labels:    labels,
		}
	}

	sa := &core.ServiceAccount{ObjectMeta: objectMeta()}
	role := &rbac.Role{ObjectMeta: objectMeta(), Rules: rules}
	binding := &rbac.RoleBinding{
		ObjectMeta: objectMeta(),
		RoleRef: rbac.RoleRef{
			APIGroup: rbac.GroupName,
			Kind:     "Role",
			Name:     name,
		},
		Subjects: []rbac.Subject{
			{
				Kind:      rbac.ServiceAccountKind,
				Name:      name,
				Namespace: kuberess.SystemNamespaceName,
			},
		},
	}

	for _, obj := range []kubemeta.MetaObject{sa, role, binding} {
		systemmeta.NoteResource(obj, kvcache.ResourceType, map[string]string{
			kvcache.ResourceNoteBackend: kvcb.Name,
			"role":                      noteRole,
		})
		kubemeta.ControlOnWithoutBlock(obj, kvcb,
			workercore.SchemeGroupVersion.WithKind("KVCacheBackend"))
	}

	return HARBAC{ServiceAccount: sa, Role: role, RoleBinding: binding}
}
