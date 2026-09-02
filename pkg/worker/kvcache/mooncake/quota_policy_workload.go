package mooncake

import (
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
	"gpustack.ai/gpustack/pkg/worker/kvcache"
)

const (
	// QuotaPolicyFileName is the key the policy document is stored under, and the file name it lands
	// as when the ConfigMap is mounted.
	//
	// It is a constant rather than a literal because TWO sides read it: whoever renders the document
	// and whoever mounts it onto the master. Those are different reconcilers, and one of them is in
	// a different spec.
	QuotaPolicyFileName = "tenant-quota-policy.yaml"

	// QuotaPolicyDir is the WRITABLE directory the master's policy file lives in, and
	// QuotaPolicyFilePath is the path the connector URI names.
	//
	// Writable is the whole requirement. The master persists an admin-API change to its connector
	// before applying it, and the file connector saves by writing a sibling temp file and renaming
	// it over the target — so the DIRECTORY has to be writable, not just the file. That rules out
	// mounting the ConfigMap here, and QuotaPolicySeedDir is where it goes instead.
	QuotaPolicyDir      = "/var/lib/mooncake"
	QuotaPolicyFilePath = QuotaPolicyDir + "/" + QuotaPolicyFileName

	// QuotaPolicySeedDir is where the ConfigMap is mounted, read once by an initContainer and never
	// by the master. It is a second location rather than the only one because the master cannot run
	// from a read-only mount, and it is not the master's because a URI pointing here would fail on
	// the first quota write instead of at startup.
	QuotaPolicySeedDir      = "/etc/mooncake/tenant-quota-policy"
	QuotaPolicySeedFilePath = QuotaPolicySeedDir + "/" + QuotaPolicyFileName

	// quotaPolicyResourceNoteRole distinguishes this object from the leader's and the members' in
	// the resource note every rendered object carries.
	quotaPolicyResourceNoteRole = "tenant-quota-policy"
)

// QuotaPolicyObjectName is the name of the ConfigMap carrying one backend's tenant quota policy.
//
// The name is derived from the BACKEND and not from any pool, which is the whole point: the policy
// file belongs to the one master serving that backend, and several pools may sit on it. A per-pool
// name would give each pool its own document, and each pass would then write a file describing only
// its own tenants — erasing the others' on a master that serves them all.
func QuotaPolicyObjectName(kvcb *workercore.KVCacheBackend) string {
	return kvcb.Name + "-tenant-quota-policy"
}

// RenderQuotaPolicyConfigMap renders the whole policy document into the ConfigMap the master's
// policy source is seeded from.
//
// The document is rendered WHOLE on every pass, never patched: it is the desired state, and the
// master rewrites its own copy on every admin-API change (F6). Re-rendering is what makes those two
// converge rather than fight — a pass that merged into what was there would be merging into the
// master's own rewrite.
//
// It carries an owner reference to the backend, so a deleted backend takes its policy with it. That
// matters because nothing else would: the ConfigMap is written by the POOL's reconciler, and a pool
// may well outlive the backend it named.
func RenderQuotaPolicyConfigMap(
	kvcb *workercore.KVCacheBackend, document []byte,
) *core.ConfigMap {
	cm := &core.ConfigMap{
		ObjectMeta: meta.ObjectMeta{
			Name:      QuotaPolicyObjectName(kvcb),
			Namespace: kuberess.SystemNamespaceName,
		},
		Data: map[string]string{
			QuotaPolicyFileName: string(document),
		},
	}

	systemmeta.NoteResource(cm, kvcache.ResourceType, map[string]string{
		kvcache.ResourceNoteBackend: kvcb.Name,
		"role":                      quotaPolicyResourceNoteRole,
	})
	kubemeta.ControlOnWithoutBlock(cm, kvcb,
		workercore.SchemeGroupVersion.WithKind("KVCacheBackend"))

	return cm
}
