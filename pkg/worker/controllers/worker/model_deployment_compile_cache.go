package worker

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	core "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/utils/stringx"
)

const (
	// modelDeploymentCompileCacheIdentityDigits is how much of a weight identity the
	// subdirectory keeps: 128 bits of the address the weights themselves have, which is beyond
	// any collision among the deployments sharing one cache root and short enough for a path.
	modelDeploymentCompileCacheIdentityDigits = 32

	// modelDeploymentCompileCacheHashDigits is how much of the compilation-factor digest the
	// leaf directory keeps: 32 bits. A collision costs one extra cold start, not a wrong
	// answer, and the leaf is where administrators prune what a flag change left behind.
	modelDeploymentCompileCacheHashDigits = 8

	// modelDeploymentCompileCacheCPUManufacturer is the manufacturer segment of a role whose
	// instance type is not acceleratable, since no vendor claims a CPU-only pool.
	modelDeploymentCompileCacheCPUManufacturer = "cpu"

	// modelDeploymentCompileCacheDigestPrefix is the algorithm prefix every content address in
	// this project carries.
	modelDeploymentCompileCacheDigestPrefix = "sha256:"

	// modelDeploymentCompileCacheOCIDigestLength is the length of the hex an OCI digest holds,
	// which is how a reference that pins one is told from one that names a mutable tag.
	modelDeploymentCompileCacheOCIDigestLength = 64
)

// ModelDeploymentCompileCacheInput holds the inputs required to compute a
// role's host compilation cache subdirectory path.
//
// Either artifact field identifies the weights, never both: the render resolves an artifact into
// ModelDeploymentArtifactRender and hands that over, while a caller reading the objects directly
// holds the ModelArtifact. A deployment naming neither downloads the weights itself, and the
// model name is then the only identity there is.
type ModelDeploymentCompileCacheInput struct {
	// Deployment is the object being rendered.
	Deployment *workercore.ModelDeployment

	// Role is the entry of Deployment.Spec.Roles this replica belongs to.
	Role *workercore.ModelDeploymentRole

	// InstanceType is the type the role's Pods are admitted against, which says what
	// manufacturer compiled the artifacts a directory may hold.
	InstanceType *worker.InstanceType

	// Artifact is how the reconciler resolved the deployment's weights, or nil for a deployment
	// naming no artifact.
	Artifact *ModelDeploymentArtifactRender

	// ModelArtifact is the artifact the deployment names, for a caller that resolved none.
	ModelArtifact *workercore.ModelArtifact
}

// BuildModelDeploymentCompileCacheSubPath returns the relative subdirectory path
// under DefaultModelDeploymentCompileCacheHostDir for a ModelDeployment role:
//
//	<manufacturer>-<engine>/<model-identity>/tp<TP>-pp<PP>-dp<DP>-ep<EP>-cp<PCP>_<DCP>-<hash8>
//
// The segments isolate what cannot share a compilation artifact: the hardware that ran the
// compiler, the engine that drove it, the weights it compiled for, the parallel shape the kernels
// were generated for, and finally a digest of everything else that reaches the generated code.
//
// The two middle levels are the prunable ones. Removing a model or a parallel shape removes every
// directory that belongs to it, and removing one leaf removes exactly the artifacts a flag change
// invalidated, which is the only granularity an engine's cache loader tolerates: vLLM's
// standalone compile adaptor reads files a manifest script names and does not survive one being
// missing underneath it.
//
// NOTE: This mapping is coupled to how vLLM (VLLM_CACHE_ROOT, including vllm-ascend)
// and SGLang (SGLANG_CACHE_DIR) organize their compiled graph and kernel caches.
// If future versions of vLLM or SGLang change their compilation cache directory
// layout, environment variable names, or compilation-affecting CLI flags, update
// the mapping and flag table in this function accordingly.
func BuildModelDeploymentCompileCacheSubPath(in ModelDeploymentCompileCacheInput) (string, error) {
	if in.Deployment == nil {
		return "", errors.New("the compilation cache path needs the deployment")
	}
	if in.Role == nil {
		return "", errors.New("the compilation cache path needs the role")
	}
	if in.InstanceType == nil {
		return "", errors.New("the compilation cache path needs the instance type")
	}

	manufacturer, err := modelDeploymentCompileCacheManufacturer(in.InstanceType)
	if err != nil {
		return "", err
	}
	engine, ok := ModelDeploymentEngineUpstreamName(in.Deployment.Spec.Engine.Name)
	if !ok {
		return "", fmt.Errorf("unsupported engine %q", in.Deployment.Spec.Engine.Name)
	}
	identity, err := modelDeploymentCompileCacheModelIdentity(in)
	if err != nil {
		return "", err
	}

	declared, err := ParseModelDeploymentDeclaredParallelism(
		in.Deployment.Spec.Engine.Name, ModelDeploymentRoleArgs(in.Role), in.Role.Env,
	)
	if err != nil {
		return "", fmt.Errorf("the compilation cache path cannot read the parallel degrees: %w", err)
	}
	digest, err := modelDeploymentCompileCacheHash(in, declared)
	if err != nil {
		return "", err
	}

	shape := fmt.Sprintf("tp%d-pp%d-dp%d-ep%d-cp%d_%d",
		modelDeploymentCompileCacheDegree(declared.TensorParallel),
		modelDeploymentCompileCacheDegree(declared.PipelineParallel),
		modelDeploymentCompileCacheDegree(declared.DataParallel),
		modelDeploymentCompileCacheDegree(declared.ExpertParallel),
		modelDeploymentCompileCacheDegree(declared.PrefillContextParallel),
		modelDeploymentCompileCacheDegree(declared.DecodeContextParallel),
	)

	return fmt.Sprintf("%s-%s/%s/%s-%s", manufacturer, engine, identity, shape,
		digest[:modelDeploymentCompileCacheHashDigits]), nil
}

// modelDeploymentCompileCacheManufacturer is the vendor whose compiler wrote the artifacts a
// directory may hold. A pool no vendor claims is a CPU pool, and calling it "generic" would
// collide with a real vendor the moment one answers for it.
func modelDeploymentCompileCacheManufacturer(it *worker.InstanceType) (string, error) {
	if !it.Spec.Acceleratable {
		return modelDeploymentCompileCacheCPUManufacturer, nil
	}

	manufacturer := strings.ToLower(strings.TrimSpace(it.Status.Detail.Manufacturer))
	if manufacturer == "" {
		return "", fmt.Errorf(
			"instance type %q is acceleratable but has no manufacturer in status", it.Name)
	}

	return manufacturer, nil
}

// modelDeploymentCompileCacheModelIdentity is the segment naming the weights, in the same
// "m-" and leading-digits shape the KV store keys carry: two deployments holding one set of
// weights address one identity, and two deployments never share a compiled artifact.
func modelDeploymentCompileCacheModelIdentity(in ModelDeploymentCompileCacheInput) (string, error) {
	switch {
	case in.ModelArtifact != nil:
		return modelDeploymentCompileCacheArtifactIdentity(in.ModelArtifact)
	case in.Artifact != nil:
		return modelDeploymentCompileCacheRenderIdentity(in)
	}

	name := strings.TrimSpace(in.Deployment.Spec.Model.Name)
	if name == "" {
		return "", errors.New("the deployment names no model, so the weights have no identity")
	}

	return modelDeploymentCompileCacheIdentityHex(
		stringx.SumBySHA256(name)), nil
}

// modelDeploymentCompileCacheArtifactIdentity is the identity of the ModelArtifact the
// deployment names, by whatever address its source carries.
func modelDeploymentCompileCacheArtifactIdentity(ma *workercore.ModelArtifact) (string, error) {
	switch {
	case ma.Spec.Source.HuggingFace != nil || ma.Spec.Source.ModelScope != nil:
		if ma.Status.Resolved != nil && ma.Status.Resolved.ManifestDigest != "" {
			hexDigits := strings.ToLower(strings.TrimPrefix(
				ma.Status.Resolved.ManifestDigest, modelDeploymentCompileCacheDigestPrefix))
			if len(hexDigits) >= modelDeploymentCompileCacheIdentityDigits &&
				modelDeploymentCompileCacheAllHex(hexDigits) {
				return modelDeploymentCompileCacheIdentityHex(hexDigits), nil
			}
		}
		// An artifact whose hub has not resolved has no content address yet. Its UID is what
		// stands in until it does, and it is stable for the life of the object rather than for
		// the weights, so a re-resolved artifact lands in a fresh directory instead of reading
		// artifacts compiled for a repository it no longer names.
		return modelDeploymentCompileCacheIdentityHex(
			stringx.SumBySHA256(string(ma.UID))), nil

	case ma.Spec.Source.Image != nil:
		reference := ma.Spec.Source.Image.Reference
		if digest, ok := modelDeploymentCompileCacheOCIDigest(reference); ok {
			return modelDeploymentCompileCacheIdentityHex(digest), nil
		}

		return modelDeploymentCompileCacheIdentityHex(
			stringx.SumBySHA256(reference)), nil

	case ma.Spec.Source.PersistentVolumeClaim != nil:
		claim := ma.Spec.Source.PersistentVolumeClaim
		return modelDeploymentCompileCacheIdentityHex(stringx.SumBySHA256(
			ma.Namespace + "/" + ma.Name + "/" + string(ma.UID) + "/" + claim.ClaimName + "/" + claim.Path,
		)), nil
	}

	return "", fmt.Errorf("model artifact %q names no source", ma.Name)
}

// modelDeploymentCompileCacheRenderIdentity is the identity of the weights as the reconciler
// resolved them for the render, which is the answer whenever the render is what is asking.
func modelDeploymentCompileCacheRenderIdentity(in ModelDeploymentCompileCacheInput) (string, error) {
	a := in.Artifact

	switch a.Delivery {
	case workercore.ModelDeploymentModelDeliveryNode, workercore.ModelDeploymentModelDeliveryEngine:
		switch {
		case a.ManifestDigest != "":
			hexDigits := strings.ToLower(strings.TrimPrefix(
				a.ManifestDigest, modelDeploymentCompileCacheDigestPrefix))
			if len(hexDigits) < modelDeploymentCompileCacheIdentityDigits ||
				!modelDeploymentCompileCacheAllHex(hexDigits) {
				return "", fmt.Errorf("the artifact manifest digest %q is not a hexadecimal address",
					a.ManifestDigest)
			}

			return modelDeploymentCompileCacheIdentityHex(hexDigits), nil
		case a.ArtifactUID != "":
			return modelDeploymentCompileCacheIdentityHex(
				stringx.SumBySHA256(a.ArtifactUID)), nil
		case a.Repository != "" || a.Revision != "":
			return modelDeploymentCompileCacheIdentityHex(stringx.SumBySHA256(
				a.Hub + "/" + a.Repository + "@" + a.Revision)), nil
		}

		// A hub artifact that resolved to nothing at all is still the repository it names, and
		// two roles pointing at one repository run the same code against the same weights.
		return modelDeploymentCompileCacheIdentityHex(
			stringx.SumBySHA256(in.Deployment.Spec.Model.Name)), nil

	case workercore.ModelDeploymentModelDeliveryImage:
		if digest, ok := modelDeploymentCompileCacheOCIDigest(a.ImageReference); ok {
			return modelDeploymentCompileCacheIdentityHex(digest), nil
		}

		return modelDeploymentCompileCacheIdentityHex(
			stringx.SumBySHA256(a.ImageReference)), nil

	case workercore.ModelDeploymentModelDeliveryPVC:
		artifactName := a.ArtifactName
		if artifactName == "" {
			artifactName = ModelDeploymentArtifactName(in.Deployment)
		}

		return modelDeploymentCompileCacheIdentityHex(stringx.SumBySHA256(
			in.Deployment.Namespace + "/" + artifactName + "/" + a.ArtifactUID + "/" +
				a.ClaimName + "/" + a.Path)), nil
	}

	return "", fmt.Errorf("unsupported model delivery %q", a.Delivery)
}

// modelDeploymentCompileCacheIdentityHex is the "m-" prefixed segment built from a hexadecimal
// address, trimmed to the digits the directory keeps.
func modelDeploymentCompileCacheIdentityHex(hexDigits string) string {
	if len(hexDigits) > modelDeploymentCompileCacheIdentityDigits {
		hexDigits = hexDigits[:modelDeploymentCompileCacheIdentityDigits]
	}

	return "m-" + hexDigits
}

// modelDeploymentCompileCacheOCIDigest returns the hexadecimal digest an image reference pins, and
// false when it names a tag instead. A tag is mutable, so it is hashed whole rather than trusted
// as an address.
func modelDeploymentCompileCacheOCIDigest(reference string) (string, bool) {
	_, digest, found := strings.Cut(reference, "@")
	if !found || !strings.HasPrefix(digest, modelDeploymentCompileCacheDigestPrefix) {
		return "", false
	}

	hexDigits := strings.ToLower(strings.TrimPrefix(digest, modelDeploymentCompileCacheDigestPrefix))
	if len(hexDigits) != modelDeploymentCompileCacheOCIDigestLength ||
		!modelDeploymentCompileCacheAllHex(hexDigits) {
		return "", false
	}

	return hexDigits, true
}

func modelDeploymentCompileCacheAllHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}

	return true
}

// modelDeploymentCompileCacheDegree is a declared degree as the directory names it: a degree
// nothing declared is the engine's own default of one, so an absent flag and an explicit 1 reach
// the same cache.
func modelDeploymentCompileCacheDegree(v int) int {
	if v <= 0 {
		return 1
	}

	return v
}

// modelDeploymentCompileCacheHashSubject is what the leaf digest is taken over. It is a struct
// rather than a map because JSON writes a struct's fields in declaration order, so the encoding of
// one set of inputs is byte-identical on every run and every build.
type modelDeploymentCompileCacheHashSubject struct {
	Image       string                                 `json:"image"`
	Parallelism modelDeploymentCompileCacheParallelism `json:"parallelism"`
	Flags       []string                               `json:"flags"`
	Env         []string                               `json:"env"`
}

// modelDeploymentCompileCacheParallelism is the parallel shape as the digest sees it. Every
// engine degree is defaulted, because the compiler generates kernels for the shape the engine
// will run and an absent degree names that shape as surely as a declared one; NodeCount stays
// raw, because zero and one are different facts about a multi-node launch and neither is the
// engine's default to borrow.
type modelDeploymentCompileCacheParallelism struct {
	TP        int      `json:"tp"`
	PP        int      `json:"pp"`
	DP        int      `json:"dp"`
	DPL       int      `json:"dpl"`
	PCP       int      `json:"pcp"`
	DCP       int      `json:"dcp"`
	EP        int      `json:"ep"`
	AttnCP    int      `json:"attnCP"`
	MoEDP     int      `json:"moeDP"`
	DWDP      int      `json:"dwdp"`
	NodeCount int      `json:"nodeCount"`
	Modes     []string `json:"modes"`
}

// modelDeploymentCompileCacheHash is the hexadecimal digest of everything about a role that can
// change the code an engine compiles.
//
// IT IS A CLOSED LIST, and that is the whole safety property: an argument left out is one a
// future flag could change the compiled output of, and including one that changes nothing costs
// a cold start every time a deployment is tuned. A flag that becomes compilation-affecting has
// to be added here deliberately, and one that stops being it has to be removed.
func modelDeploymentCompileCacheHash(
	in ModelDeploymentCompileCacheInput, declared ModelDeploymentDeclaredParallelism,
) (string, error) {
	image := strings.TrimSpace(in.Role.Image)
	if image == "" {
		image = strings.TrimSpace(in.Deployment.Spec.Engine.Image)
	}
	modes := slices.Clone(declared.Modes)
	slices.Sort(modes)

	subject := modelDeploymentCompileCacheHashSubject{
		Image: image,
		Parallelism: modelDeploymentCompileCacheParallelism{
			TP:        modelDeploymentCompileCacheDegree(declared.TensorParallel),
			PP:        modelDeploymentCompileCacheDegree(declared.PipelineParallel),
			DP:        modelDeploymentCompileCacheDegree(declared.DataParallel),
			DPL:       modelDeploymentCompileCacheDegree(declared.DataParallelLocal),
			PCP:       modelDeploymentCompileCacheDegree(declared.PrefillContextParallel),
			DCP:       modelDeploymentCompileCacheDegree(declared.DecodeContextParallel),
			EP:        modelDeploymentCompileCacheDegree(declared.ExpertParallel),
			AttnCP:    modelDeploymentCompileCacheDegree(declared.AttentionContextParallel),
			MoEDP:     modelDeploymentCompileCacheDegree(declared.MoEDataParallel),
			DWDP:      modelDeploymentCompileCacheDegree(declared.DWDPSize),
			NodeCount: declared.NodeCount,
			Modes:     modes,
		},
		Flags: modelDeploymentCompileCacheFlags(ModelDeploymentRoleArgs(in.Role)),
		Env:   modelDeploymentCompileCacheEnv(in.Role.Env),
	}

	canonical, err := json.Marshal(subject)
	if err != nil {
		return "", fmt.Errorf("the compilation cache path cannot encode its factors: %w", err)
	}

	return stringx.SumBytesBySHA256(canonical), nil
}

// modelDeploymentCompileCacheFlag is one row of the flag table: the canonical name an entry
// normalizes to, and how much of the stream the flag consumes with it.
type modelDeploymentCompileCacheFlag struct {
	// valued marks a flag whose value reaches the generated code. A switch takes no value at
	// all, which is what keeps the token after it from being swallowed.
	valued bool

	// multi marks a valued flag taking every consecutive token that is not itself a flag, as
	// vLLM's own parser does for its capture-size lists.
	multi bool
}

// modelDeploymentCompileCacheOptimizationFlag is the short spelling vLLM compiles at, which is
// also the table row it is keyed under.
const modelDeploymentCompileCacheOptimizationFlag = "-O"

// modelDeploymentCompileCacheQuantizationFlag is the short spelling of --quantization.
const modelDeploymentCompileCacheQuantizationFlag = "-q"

// modelDeploymentCompileCacheFlagTable is the compilation-affecting flag table, keyed by the
// canonical name the directory is hashed under.
//
// Everything in it selects something the compiler sees: the dtype and quantization it generates
// kernels for, the context length that fixes graph shapes, the compilation and graph capture
// settings themselves, the attention and speculative-decoding backends, and the model structure
// overrides that change the graph before it is traced. Serving flags are absent by construction:
// how many requests a replica admits, how much of its memory it may hold and what it logs change
// no kernel, and including them would give every tuning knob a fresh cold start.
var modelDeploymentCompileCacheFlagTable = map[string]modelDeploymentCompileCacheFlag{
	// Data type and quantization.
	"--dtype":                   {valued: true},
	"--quantization":            {valued: true},
	"--quantization-param-path": {valued: true},
	"--kv-cache-dtype":          {valued: true},
	"--moe-runner-backend":      {valued: true},
	// Context length.
	"--max-model-len":  {valued: true},
	"--context-length": {valued: true},
	// Compilation and graph capture.
	"--compilation-config":                      {valued: true},
	modelDeploymentCompileCacheOptimizationFlag: {valued: true},
	"--enforce-eager":                           {},
	"--cudagraph-capture-sizes":                 {valued: true, multi: true},
	"--max-cudagraph-capture-size":              {valued: true},
	"--additional-config":                       {valued: true},
	"--enable-torch-compile":                    {},
	"--torch-compile-max-bs":                    {valued: true},
	"--cuda-graph-max-bs":                       {valued: true},
	"--cuda-graph-bs":                           {valued: true, multi: true},
	"--disable-cuda-graph":                      {},
	"--disable-cuda-graph-padding":              {},
	"--piecewise-cuda-graph":                    {},
	"--piecewise-cuda-graph-max-tokens":         {valued: true},
	"--piecewise-cuda-graph-tokens":             {valued: true, multi: true},
	"--piecewise-cuda-graph-compiler":           {valued: true},
	// Attention and speculative decoding.
	"--attention-backend":               {valued: true},
	"--attention-config":                {valued: true},
	"--prefill-attention-backend":       {valued: true},
	"--decode-attention-backend":        {valued: true},
	"--triton-attention-reduce-in-fp32": {},
	"--all2all-backend":                 {valued: true},
	"--speculative-config":              {valued: true},
	"--speculative-algorithm":           {valued: true},
	"--speculative-num-steps":           {valued: true},
	"--speculative-eagle-topk":          {valued: true},
	"--speculative-num-draft-tokens":    {valued: true},
	// Model structure overrides.
	"--trust-remote-code":        {},
	"--hf-overrides":             {valued: true},
	"--json-model-override-args": {valued: true},
	"--rope-scaling":             {valued: true},
	"--rope-theta":               {valued: true},
	"--config":                   {valued: true},
}

// modelDeploymentCompileCacheFlagAliases maps a spelling the engines accept onto its canonical
// table name.
var modelDeploymentCompileCacheFlagAliases = map[string]string{
	modelDeploymentCompileCacheQuantizationFlag: "--quantization",
}

// modelDeploymentCompileCacheFlags returns the "key=value" pairs of one role's argument stream
// that the compilation cache is keyed by, sorted.
//
// Last wins on a repeated name, as argparse resolves it: the engine runs the last value, so the
// cache must be the one compiled for the last value. Sorting is what makes the pair of unrelated
// flags order-independent, so appending an argument to a role does not silently rebuild a
// directory nobody edited anything in.
func modelDeploymentCompileCacheFlags(args []string) []string {
	pairs := map[string]string{}

	for i := 0; i < len(args); i++ {
		// "--" alone ends the options, and everything past it is the program's own.
		if args[i] == "--" {
			break
		}
		key, values, consumed, ok := modelDeploymentCompileCacheReadFlag(args, i)
		if !ok {
			continue
		}
		i += consumed
		pairs[key] = strings.Join(values, ",")
	}

	out := make([]string, 0, len(pairs))
	for key, value := range pairs {
		out = append(out, key+"="+value)
	}
	slices.Sort(out)

	return out
}

// modelDeploymentCompileCacheReadFlag reads one token of an argument stream as a table row and
// returns the canonical key, the values it resolved to, and how many further tokens it consumed.
func modelDeploymentCompileCacheReadFlag(
	args []string, i int,
) (string, []string, int, bool) {
	token := args[i]
	// A bare "-" is a stream an engine reads as the program to run rather than as an option.
	if !strings.HasPrefix(token, "-") || token == "-" {
		return "", nil, 0, false
	}

	key, value, hasValue := token, "", false
	if eq := strings.IndexByte(token, '='); eq >= 0 {
		key, value, hasValue = token[:eq], token[eq+1:], true
	}

	canonical, glued, ok := modelDeploymentCompileCacheFlagKey(key)
	if !ok {
		return "", nil, 0, false
	}
	if !hasValue && glued != "" {
		value, hasValue = glued, true
	}

	// The row a key is read by is the flag before its first dot: "--compilation-config.level=3"
	// is one value of the --compilation-config flag, not a flag the table has never heard of.
	row := modelDeploymentCompileCacheFlagTable[canonical]
	if base, _, dotted := strings.Cut(canonical, "."); dotted {
		row = modelDeploymentCompileCacheFlagTable[base]
	}
	if !row.valued {
		if hasValue {
			// A switch carrying a value is the engine's own refusal, so it passes through
			// unrecorded rather than keying a cache for a command line that will not run.
			return "", nil, 0, false
		}

		return canonical, []string{"true"}, 0, true
	}

	values, consumed := []string{}, 0
	switch {
	case hasValue:
		values = []string{value}
	case row.multi:
		for i+1+consumed < len(args) && modelDeploymentConsumableValue(args[i+1+consumed]) {
			consumed++
			values = append(values, args[i+1+consumed-1])
		}
	case i+1 < len(args) && modelDeploymentConsumableValue(args[i+1]):
		consumed = 1
		values = []string{args[i+1]}
	}
	if len(values) == 0 {
		// A valued flag with nothing after it is the engine's own refusal. It keys its own
		// directory rather than folding into whatever the last good value compiled, so a broken
		// command line never reads another one's artifacts.
		values = []string{""}
	}

	return canonical, values, consumed, true
}

// modelDeploymentCompileCacheFlagKey resolves one flag key to its table row, and reports the value
// argparse would read as glued to it.
//
// The two normalization rules are the engines'. vLLM rewrites the underscores of a long flag key
// to dashes up to its first dot and merges a dotted entry into the flag before the dot, so
// "--compilation_config.level=3" and "--compilation-config.level=3" are one spelling; a dotted key
// keeps its suffix, because two overrides of different fields are two changes and neither
// replaces the other. A short option is resolved by argparse's own rule rather than by the long
// flag's, since "-q" is quantization whatever any long flag starts with.
func modelDeploymentCompileCacheFlagKey(key string) (canonical, glued string, ok bool) {
	if !strings.HasPrefix(key, "--") {
		return modelDeploymentCompileCacheShortFlag(key)
	}

	name := modelDeploymentVLLMFlagKey(key)
	if base, _, dotted := strings.Cut(name, "."); dotted {
		if _, known := modelDeploymentCompileCacheFlagTable[base]; !known {
			return "", "", false
		}

		return name, "", true
	}

	if alias, aliased := modelDeploymentCompileCacheFlagAliases[name]; aliased {
		return alias, "", true
	}
	if _, known := modelDeploymentCompileCacheFlagTable[name]; known {
		return name, "", true
	}
	// A prefix resolves the way argparse resolves it, counted over this table's spellings.
	// IT MUST: an unresolved flag is an unrecorded one, and a role spelling "--dtype" as "--dt"
	// would then share the directory compiled for a different dtype.
	//
	// A prefix this table finds AMBIGUOUS stays unresolved, which is the loud direction. The
	// engine registers more flags than this table names, so an ambiguous prefix here is one the
	// engine refuses at startup, and guessing past it would key a directory on a flag no run ever
	// passed. A dotted key is not abbreviated: vLLM writes those as whole documents.
	if !strings.Contains(name, ".") {
		if canonical, unique := modelDeploymentCompileCacheFlagByPrefix(name); unique {
			return canonical, "", true
		}
	}

	return "", "", false
}

// modelDeploymentCompileCacheFlagByPrefix resolves a key to the one table name it abbreviates.
func modelDeploymentCompileCacheFlagByPrefix(name string) (string, bool) {
	matched := ""
	for candidate := range modelDeploymentCompileCacheFlagTable {
		if !strings.HasPrefix(candidate, name) {
			continue
		}
		if matched != "" {
			return "", false
		}
		matched = candidate
	}

	return matched, matched != ""
}

// modelDeploymentCompileCacheShortFlag resolves a single-dash key. "-O3" and "-qfp8" carry their
// value on the token itself, which is argparse's rule for a short option, while "-O" and "-q" take
// the next token instead.
func modelDeploymentCompileCacheShortFlag(key string) (string, string, bool) {
	optimization := modelDeploymentCompileCacheOptimizationFlag
	quantization := modelDeploymentCompileCacheQuantizationFlag

	switch key {
	case optimization:
		return optimization, "", true
	case quantization:
		return modelDeploymentCompileCacheFlagAliases[quantization], "", true
	}

	if rest := strings.TrimPrefix(key, optimization); rest != key {
		if modelDeploymentAllDigits(rest) {
			return optimization, rest, true
		}

		return "", "", false
	}
	if rest := strings.TrimPrefix(key, quantization); rest != key && rest != "" {
		return modelDeploymentCompileCacheFlagAliases[quantization], rest, true
	}

	return "", "", false
}

// modelDeploymentCompileCacheEnvNames are the environment entries that reach what an engine
// compiles: the switches that choose a compile path, the attention backend they select, the
// FlashAttention build they resolve, and the architecture list the kernels are generated for.
//
// Everything else an engine reads through the environment -- a Hub token, a cache root, a proxy
// -- changes where it reads from, never what it generates, and including it would hand every
// endpoint change a cold start.
var modelDeploymentCompileCacheEnvNames = []string{
	"VLLM_USE_V1",
	"VLLM_ATTENTION_BACKEND",
	"VLLM_USE_STANDALONE_COMPILE",
	"VLLM_USE_MEGA_AOT_ARTIFACT",
	"VLLM_FLASH_ATTN_VERSION",
	"SGLANG_ENABLE_TORCH_COMPILE",
	"TRITON_ATTN_NUM_STAGES",
	"TORCH_CUDA_ARCH_LIST",
}

// modelDeploymentCompileCacheEnv returns the "NAME=VALUE" pairs of one role's environment that the
// compilation cache is keyed by, sorted. A repeated name is read last-wins, the order the
// container runtime applies it in.
func modelDeploymentCompileCacheEnv(env []workercore.ModelDeploymentEnvVar) []string {
	var out []string
	for _, name := range modelDeploymentCompileCacheEnvNames {
		if value, ok := modelDeploymentEnvValue(env, name); ok {
			out = append(out, name+"="+value)
		}
	}
	slices.Sort(out)

	return out
}

// modelDeploymentCompileCacheVolumes returns the HostPath (DirectoryOrCreate) volume
// and container VolumeMount for a managed role's compilation cache.
func modelDeploymentCompileCacheVolumes(subPath string) (core.Volume, core.VolumeMount) {
	return core.Volume{
			Name: ModelDeploymentCompileCacheVolumeName,
			VolumeSource: core.VolumeSource{HostPath: &core.HostPathVolumeSource{
				Path: path.Join(DefaultModelDeploymentCompileCacheHostDir, subPath),
				Type: ptr.To(core.HostPathDirectoryOrCreate),
			}},
		}, core.VolumeMount{
			Name:      ModelDeploymentCompileCacheVolumeName,
			MountPath: ModelDeploymentCompileCacheMountPath,
		}
}

// modelDeploymentCompileCacheMountOverridden reports whether an existing container mount already
// covers ModelDeploymentCompileCacheMountPath (at, inside, or around it), in which case the
// operator yields its default host-backed compilation cache volume to the user's mount.
func modelDeploymentCompileCacheMountOverridden(mounts []core.VolumeMount) bool {
	return slices.ContainsFunc(mounts, func(m core.VolumeMount) bool {
		return modelDeploymentCompileCachePathsOverlap(m.MountPath, ModelDeploymentCompileCacheMountPath)
	})
}

func modelDeploymentCompileCachePathsOverlap(a, b string) bool {
	a, b = path.Clean(a), path.Clean(b)

	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

// modelDeploymentCompileCacheDefaultedEnv returns the defaulted environment variable
// pointing the engine's compilation cache to ModelDeploymentCompileCacheMountPath.
//
// IT IS THE CACHE ROOT ALONE. The operator does not name the directory an engine builds inside
// it, because vLLM partitions that tree by rank and DP rank itself and SGLang does not: writing
// past their partitioning would put two ranks in one directory and have them overwrite each
// other's generated code. An engine with no such variable gets nothing rather than a wrong one.
func modelDeploymentCompileCacheDefaultedEnv(engine string) []core.EnvVar {
	switch engine {
	case workercore.ModelDeploymentEngineVLLM:
		return []core.EnvVar{{Name: "VLLM_CACHE_ROOT", Value: ModelDeploymentCompileCacheMountPath}}
	case workercore.ModelDeploymentEngineSGLang:
		return []core.EnvVar{{Name: "SGLANG_CACHE_DIR", Value: ModelDeploymentCompileCacheMountPath}}
	}

	return nil
}
