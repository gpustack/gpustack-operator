package worker

import (
	"path"

	core "k8s.io/api/core/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// The environment names an engine's startup caches honor, and the subdirectory of the cache
// mount each one owns.
//
// An engine opens several caches before it serves its first token, and each of them defaults to
// a path on the container's writable layer. The pod's ephemeral-storage limit measures that
// layer, so a cache that grows there evicts the pod that is filling it, while the same bytes on
// the host-backed cache mount do not count against the limit at all.
//
// The four names below are the distinct roots those caches take at vLLM v0.32.0:
//
//   - XDG_CACHE_HOME is the root the XDG Base Directory Specification puts a user's caches
//     under. It is how the Hugging Face hub cache, the tokenizer files, the Hub token, the Xet
//     cache, the torch hub directory and the pip cache find their default location: the
//     Hugging Face library derives its home from it, so one name moves that whole tree.
//   - FLASHINFER_WORKSPACE_BASE is the base FlashInfer builds its JIT workspace under. The
//     workspace holds the generated sources and the compiled kernels of the attention backend,
//     which the engine builds while it starts. FlashInfer appends its own ".cache/flashinfer"
//     to the base, so the tree lands under <mount>/flashinfer/.cache/flashinfer.
//   - TORCHINDUCTOR_CACHE_DIR is where torch keeps the kernels inductor compiles. Its default
//     is a directory under the temporary directory, which is the container's writable layer.
//   - TRITON_CACHE_DIR is where triton's compiled kernels are kept. Its default is under the
//     home directory, beside the cache root rather than inside it.
//
// NONE OF THESE IS THE ENGINE'S OWN CACHE ROOT. That one is VLLM_CACHE_ROOT or SGLANG_CACHE_DIR,
// rendered by modelDeploymentCompileCacheDefaultedEnv, and this table does not repeat it.
//
// NOTE: this mapping is coupled to where those libraries put their caches and to the variable
// each of them reads. If a future version of vLLM, torch, triton, flashinfer or the Hugging
// Face library changes a default location or a variable name, update this table accordingly.
const (
	// modelDeploymentXDGCacheHomeEnv is the XDG Base Directory Specification's cache root.
	modelDeploymentXDGCacheHomeEnv = "XDG_CACHE_HOME"

	// modelDeploymentFlashInferWorkspaceBaseEnv is the base FlashInfer builds its JIT
	// workspace under.
	modelDeploymentFlashInferWorkspaceBaseEnv = "FLASHINFER_WORKSPACE_BASE"

	// modelDeploymentTorchInductorCacheDirEnv is where torch keeps the kernels inductor
	// compiles.
	modelDeploymentTorchInductorCacheDirEnv = "TORCHINDUCTOR_CACHE_DIR"

	// modelDeploymentTritonCacheDirEnv is where triton keeps its compiled kernels.
	modelDeploymentTritonCacheDirEnv = "TRITON_CACHE_DIR"
)

// modelDeploymentRuntimeCache is one cache environment name and the subdirectory of the cache
// mount it owns.
type modelDeploymentRuntimeCache struct {
	// Name is the environment variable the library reads.
	Name string

	// SubDir is the directory under the cache mount the name is rendered to.
	SubDir string
}

// modelDeploymentRuntimeCaches is the table of the caches an engine opens while it starts, in
// the order they are rendered. The order is fixed so that a rendered environment is the same
// document every time.
var modelDeploymentRuntimeCaches = []modelDeploymentRuntimeCache{
	{Name: modelDeploymentXDGCacheHomeEnv, SubDir: "xdg"},
	{Name: modelDeploymentFlashInferWorkspaceBaseEnv, SubDir: "flashinfer"},
	{Name: modelDeploymentTorchInductorCacheDirEnv, SubDir: "inductor"},
	{Name: modelDeploymentTritonCacheDirEnv, SubDir: "triton"},
}

// modelDeploymentRuntimeCacheEnvNames are the names of the table, for the registry that
// decides which environment variables the operator merely defaults.
var modelDeploymentRuntimeCacheEnvNames = []string{
	modelDeploymentXDGCacheHomeEnv,
	modelDeploymentFlashInferWorkspaceBaseEnv,
	modelDeploymentTorchInductorCacheDirEnv,
	modelDeploymentTritonCacheDirEnv,
}

// modelDeploymentRuntimeCacheDefaultedEnv returns the defaulted environment entries that point
// an engine's startup caches at the host-backed cache mount.
//
// IT IS A DEFAULT AND NOT AN OWNERSHIP. A role that sets one of these names keeps its own
// value: the operator claims only the default, and the volume mounted at
// ModelDeploymentCompileCacheMountPath is what makes that default true.
//
// An engine the operator does not know gets nothing rather than a wrong path. The names belong
// to the libraries an engine may or may not carry, so a name the operator cannot vouch for
// would be a claim about an engine it has never seen.
func modelDeploymentRuntimeCacheDefaultedEnv(engine string) []core.EnvVar {
	switch engine {
	case workercore.ModelDeploymentEngineVLLM, workercore.ModelDeploymentEngineSGLang:
	default:
		return nil
	}

	env := make([]core.EnvVar, 0, len(modelDeploymentRuntimeCaches))
	for _, c := range modelDeploymentRuntimeCaches {
		env = append(env, core.EnvVar{
			Name:  c.Name,
			Value: path.Join(ModelDeploymentCompileCacheMountPath, c.SubDir),
		})
	}

	return env
}
