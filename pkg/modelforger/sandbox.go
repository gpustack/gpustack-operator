package modelforger

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	kvalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"
	kueuectrlconst "sigs.k8s.io/kueue/pkg/controller/constants"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/modelkernelkey"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/utils/json"
)

const (
	// InputMountPath is where the read-only forge-input ConfigMap is mounted inside ForgeSandbox.
	InputMountPath = "/var/run/gpustack/forge/input"
	// OutputMountPath is where the forge-output emptyDir is mounted inside ForgeSandbox.
	OutputMountPath = "/var/run/gpustack/forge/output"
	// VerifiedMountPath is where the forge-verified emptyDir is mounted inside ForgeSandbox.
	VerifiedMountPath = "/var/run/gpustack/forge/verified"
	// SentinelMountPath is where the staged engine-sentinel Python package is mounted in External mode.
	SentinelMountPath = "/var/run/gpustack/forge/sentinel"
	// OperatorSentinelSourceDir is where the unpacked engine-sentinel package lives in the operator image.
	OperatorSentinelSourceDir = "/etc/gpustack/lib/engine-sentinel"

	// InputVolumeName names the read-only input ConfigMap volume on ForgeSandbox.
	InputVolumeName = "forge-input"
	// OutputVolumeName names the emptyDir staging volume written by forge-runner and read by forge-verifier.
	OutputVolumeName = "forge-output"
	// VerifiedVolumeName names the emptyDir volume where forge-verifier writes verification-report.json.
	VerifiedVolumeName = "forge-verified"
	// SentinelVolumeName names the emptyDir volume staging gpustack-engine-sentinel in External mode.
	SentinelVolumeName = "forge-sentinel"

	// RunnerContainerName names the synthesis initContainer inside ForgeSandbox.
	RunnerContainerName = "forge-runner"
	// StageSentinelContainerName names the sentinel-staging initContainer used in External mode.
	StageSentinelContainerName = "forge-stage-sentinel"
	// VerifierContainerName names the independent verification initContainer inside ForgeSandbox.
	VerifierContainerName = "forge-verifier"
	// HoldContainerName names the bounded main container holding the Pod alive during ingestion.
	HoldContainerName = "forge-hold"

	// MaxInputConfigMapBytes bounds the serialized forge-input ConfigMap size.
	MaxInputConfigMapBytes = 512 << 10
	// DefaultSandboxTimeoutSeconds is the default ActiveDeadlineSeconds for ForgeSandbox Pods.
	DefaultSandboxTimeoutSeconds int64 = 1800
	// DefaultHoldMaxDuration is the default maximum hold window for gpustack-operator model-forger hold.
	DefaultHoldMaxDuration = 10 * time.Minute

	// SandboxLabelName identifies ForgeSandbox Pods and ConfigMaps.
	SandboxLabelName = "app.kubernetes.io/name"
	// SandboxLabelInstance carries the owning ModelDeploymentForger name.
	SandboxLabelInstance = "app.kubernetes.io/instance"
	// SandboxLabelComponent identifies ForgeSandbox component objects.
	SandboxLabelComponent = "app.kubernetes.io/component"
	// SandboxLabelRole carries the targeted ModelDeployment role name.
	SandboxLabelRole = "forger.gpustack.ai/role"
	// SandboxLabelGeneration carries the 1-based forge generation number.
	SandboxLabelGeneration = "forger.gpustack.ai/generation"

	sandboxLabelValueName      = "model-deployment-forger"
	sandboxLabelValueComponent = "forge-sandbox"

	envForgeInputDir          = "GPUSTACK_FORGE_INPUT_DIR"
	envForgeOutputDir         = "GPUSTACK_FORGE_OUTPUT_DIR"
	reservedForgeEnvPrefix    = "GPUSTACK_FORGE_"
	envEngineSentinelEnable   = "GPUSTACK_ENGINE_SENTINEL_ENABLE"
	envPythonPath             = "PYTHONPATH"
	configMapDefinitionPrefix = "def-"
	configMapWorkloadPrefix   = "wl-"
)

var (
	defaultOutputSizeLimitQty = resource.MustParse("2Gi")
	verifiedSizeLimitQty      = resource.MustParse("16Mi")
	sentinelSizeLimitQty      = resource.MustParse("16Mi")
	safeConfigMapKeyCharRe    = regexp.MustCompile(`[^A-Za-z0-9._-]+`)
)

// InputConfigMapSpec holds the inputs used to render a ForgeSandbox input ConfigMap.
type InputConfigMapSpec struct {
	Forger      *workercore.ModelDeploymentForger
	Name        string
	RoleName    string
	Generation  int64
	Request     ForgeRequest
	Definitions []Definition
	Workloads   []Workload
}

type encodedWorkload struct {
	workload Workload
	line     []byte
	order    int
}

func shouldEvictWorkloadBefore(a, b encodedWorkload) bool {
	if a.workload.ObservedDurationUs != b.workload.ObservedDurationUs {
		return a.workload.ObservedDurationUs < b.workload.ObservedDurationUs
	}
	if a.workload.UUID != b.workload.UUID {
		return a.workload.UUID > b.workload.UUID
	}
	return a.order > b.order
}

func evictNextWorkload(workloadsByDef map[string][]encodedWorkload, wlGroupNames []string) (int, bool) {
	bestGroup := ""
	bestIdx := -1
	var best encodedWorkload
	for _, safeName := range wlGroupNames {
		group := workloadsByDef[safeName]
		if len(group) <= 1 {
			continue
		}
		for idx, cand := range group {
			if bestIdx == -1 || shouldEvictWorkloadBefore(cand, best) {
				bestGroup = safeName
				bestIdx = idx
				best = cand
			}
		}
	}
	if bestIdx == -1 {
		return 0, false
	}
	workloadsByDef[bestGroup] = slices.Delete(workloadsByDef[bestGroup], bestIdx, bestIdx+1)
	return len(best.line), true
}

// BuildInputConfigMap builds an owner-referenced ConfigMap (<= 512 KiB) carrying
// forge-request.json, definitions/<name>.json, and workloads/<name>.jsonl along with the sorted
// KeyToPath projections required to mount the nested layout into /var/run/gpustack/forge/input/.
func BuildInputConfigMap(spec InputConfigMapSpec) (*core.ConfigMap, []core.KeyToPath, error) {
	if spec.Forger == nil {
		return nil, nil, errors.New("forger must not be nil")
	}
	if strings.TrimSpace(spec.Forger.Namespace) == "" || strings.TrimSpace(spec.Forger.Name) == "" {
		return nil, nil, errors.New("forger namespace and name must not be empty")
	}
	if strings.TrimSpace(spec.Name) == "" {
		return nil, nil, errors.New("input configMap name must not be empty")
	}
	if strings.TrimSpace(spec.RoleName) == "" {
		return nil, nil, errors.New("roleName must not be empty")
	}
	if spec.Generation < 1 {
		return nil, nil, fmt.Errorf("generation %d must be >= 1", spec.Generation)
	}

	reqCopy := spec.Request
	if reqCopy.SchemaVersion == "" {
		reqCopy.SchemaVersion = ForgeRequestSchemaVersion
	}
	if err := ValidateForgeRequest(&reqCopy); err != nil {
		return nil, nil, fmt.Errorf("validate forge request: %w", err)
	}

	data := make(map[string]string, 1+len(spec.Definitions)*2)
	items := make([]core.KeyToPath, 0, 1+len(spec.Definitions)*2)

	reqBytes, err := marshalCompactJSON(reqCopy)
	if err != nil {
		return nil, nil, fmt.Errorf("encode %s: %w", ForgeRequestFileName, err)
	}
	data[ForgeRequestFileName] = string(reqBytes)
	items = append(items, core.KeyToPath{
		Key:  ForgeRequestFileName,
		Path: ForgeRequestFileName,
	})

	usedDefNames := make(map[string]struct{}, len(spec.Definitions))
	for i := range spec.Definitions {
		def := &spec.Definitions[i]
		if err := ValidateDefinition(def); err != nil {
			return nil, nil, fmt.Errorf("definitions[%d]: %w", i, err)
		}
		safeName := sanitizeConfigMapSegment(def.Name)
		if _, dup := usedDefNames[safeName]; dup {
			return nil, nil, fmt.Errorf("duplicate sanitized definition name %q", safeName)
		}
		usedDefNames[safeName] = struct{}{}

		defBytes, err := marshalCompactJSON(def)
		if err != nil {
			return nil, nil, fmt.Errorf("encode definition %q: %w", def.Name, err)
		}
		cmKey := configMapDefinitionPrefix + safeName + ".json"
		data[cmKey] = string(defBytes)
		items = append(items, core.KeyToPath{
			Key:  cmKey,
			Path: "definitions/" + safeName + ".json",
		})
	}

	workloadsByDef := make(map[string][]encodedWorkload)
	for i := range spec.Workloads {
		wl := spec.Workloads[i]
		if err := ValidateWorkload(&wl); err != nil {
			return nil, nil, fmt.Errorf("workloads[%d]: %w", i, err)
		}
		line, err := marshalCompactJSON(wl)
		if err != nil {
			return nil, nil, fmt.Errorf("encode workload %q: %w", wl.UUID, err)
		}
		defKey := strings.TrimSpace(wl.Definition)
		if defKey == "" {
			defKey = strings.TrimSpace(wl.LocalKernelKey)
		}
		if defKey == "" {
			defKey = "default"
		}
		safeName := sanitizeConfigMapSegment(defKey)
		workloadsByDef[safeName] = append(workloadsByDef[safeName], encodedWorkload{
			workload: wl,
			line:     line,
			order:    i,
		})
	}

	totalBytes := 0
	for k, v := range data {
		totalBytes += len(k) + len(v)
	}
	wlGroupNames := slices.Sorted(maps.Keys(workloadsByDef))
	for _, safeName := range wlGroupNames {
		cmKey := configMapWorkloadPrefix + safeName + ".jsonl"
		totalBytes += len(cmKey)
		for _, entry := range workloadsByDef[safeName] {
			totalBytes += len(entry.line) + 1
		}
	}

	evictedWorkloads := 0
	for totalBytes > MaxInputConfigMapBytes {
		lineLen, ok := evictNextWorkload(workloadsByDef, wlGroupNames)
		if !ok {
			break
		}
		totalBytes -= lineLen + 1
		evictedWorkloads++
	}

	if evictedWorkloads > 0 {
		for {
			reqCopy.EvictedWorkloads = evictedWorkloads
			updatedReqBytes, err := marshalCompactJSON(reqCopy)
			if err != nil {
				return nil, nil, fmt.Errorf("encode forge request with eviction metadata: %w", err)
			}
			totalBytes += len(updatedReqBytes) - len(data[ForgeRequestFileName])
			data[ForgeRequestFileName] = string(updatedReqBytes)
			if totalBytes <= MaxInputConfigMapBytes {
				break
			}
			lineLen, ok := evictNextWorkload(workloadsByDef, wlGroupNames)
			if !ok {
				break
			}
			totalBytes -= lineLen + 1
			evictedWorkloads++
		}
	}

	for _, safeName := range wlGroupNames {
		var buf bytes.Buffer
		for _, entry := range workloadsByDef[safeName] {
			buf.Write(entry.line)
			buf.WriteByte('\n')
		}
		cmKey := configMapWorkloadPrefix + safeName + ".jsonl"
		data[cmKey] = buf.String()
		items = append(items, core.KeyToPath{
			Key:  cmKey,
			Path: "workloads/" + safeName + ".jsonl",
		})
	}

	if totalBytes > MaxInputConfigMapBytes {
		return nil, nil, fmt.Errorf("input configMap size %d bytes exceeds %d byte cap",
			totalBytes, MaxInputConfigMapBytes)
	}

	slices.SortFunc(items, func(a, b core.KeyToPath) int {
		return strings.Compare(a.Key, b.Key)
	})

	cm := &core.ConfigMap{
		ObjectMeta: meta.ObjectMeta{
			Name:            spec.Name,
			Namespace:       spec.Forger.Namespace,
			Labels:          sandboxCommonLabels(spec.Forger.Name, spec.RoleName, spec.Generation),
			OwnerReferences: []meta.OwnerReference{forgerControllerOwnerRef(spec.Forger)},
		},
		Immutable: ptr.To(true),
		Data:      data,
	}
	return cm, items, nil
}

// ConfigMapVolumeItems reconstructs sorted KeyToPath projections from a ForgeSandbox input ConfigMap.
func ConfigMapVolumeItems(cm *core.ConfigMap) []core.KeyToPath {
	if cm == nil || len(cm.Data) == 0 {
		return nil
	}
	keys := slices.Sorted(maps.Keys(cm.Data))
	items := make([]core.KeyToPath, 0, len(keys))
	for _, key := range keys {
		relPath := key
		switch {
		case strings.HasPrefix(key, configMapDefinitionPrefix):
			relPath = "definitions/" + strings.TrimPrefix(key, configMapDefinitionPrefix)
		case strings.HasPrefix(key, configMapWorkloadPrefix):
			relPath = "workloads/" + strings.TrimPrefix(key, configMapWorkloadPrefix)
		}
		items = append(items, core.KeyToPath{Key: key, Path: relPath})
	}
	return items
}

// SandboxPodSpec holds the inputs used to build a Kueue-admitted restricted-PSA ForgeSandbox Pod.
type SandboxPodSpec struct {
	Forger                  *workercore.ModelDeploymentForger
	Name                    string
	RoleName                string
	Generation              int64
	InputConfigMapName      string
	InputConfigMapItems     []core.KeyToPath
	EntranceQueue           string
	AcceleratorType         *workercore.InstanceType
	Manufacturer            string
	AcceleratorResourceName core.ResourceName
	Hardware                modelkernelkey.NodeHardware
	ExtraNodeSelector       map[string]string
	OperatorImage           string
	EngineImage             string
	SandboxSpec             workercore.ModelDeploymentForgerSandboxSpec
	Thresholds              *VerificationThresholds
	HoldMaxDuration         time.Duration
	ImagePullSecrets        []core.LocalObjectReference
}

// BuildSandboxPod builds a Kueue-admitted, restricted Pod Security compliant ForgeSandbox Pod with
// a single accelerator card, slice, or hardware partition, GPU architecture nodeSelector, emptyDir
// staging volumes, and zero hostPath volumes.
func BuildSandboxPod(spec SandboxPodSpec) (*core.Pod, error) {
	if spec.Forger == nil {
		return nil, errors.New("forger must not be nil")
	}
	if strings.TrimSpace(spec.Forger.Namespace) == "" || strings.TrimSpace(spec.Forger.Name) == "" {
		return nil, errors.New("forger namespace and name must not be empty")
	}
	if strings.TrimSpace(spec.Name) == "" {
		return nil, errors.New("sandbox pod name must not be empty")
	}
	if strings.TrimSpace(spec.RoleName) == "" {
		return nil, errors.New("roleName must not be empty")
	}
	if spec.Generation < 1 {
		return nil, fmt.Errorf("generation %d must be >= 1", spec.Generation)
	}
	if strings.TrimSpace(spec.InputConfigMapName) == "" {
		return nil, errors.New("inputConfigMapName must not be empty")
	}
	entrance := strings.TrimSpace(spec.EntranceQueue)
	if entrance == "" {
		return nil, errors.New("entranceQueue is required for Kueue admission")
	}
	operatorImage := strings.TrimSpace(spec.OperatorImage)
	if operatorImage == "" {
		return nil, errors.New("operatorImage must not be empty")
	}

	runnerMode := spec.SandboxSpec.RunnerMode
	if runnerMode == "" {
		runnerMode = workercore.ModelDeploymentForgerRunnerModeExternal
	}
	switch runnerMode {
	case workercore.ModelDeploymentForgerRunnerModeMock:
		if spec.SandboxSpec.Runner != nil {
			return nil, errors.New("sandbox.runner must not be set when runnerMode is Mock")
		}
	case workercore.ModelDeploymentForgerRunnerModeExternal:
		if spec.SandboxSpec.Runner == nil || strings.TrimSpace(spec.SandboxSpec.Runner.Image) == "" {
			return nil, errors.New("sandbox.runner.image is required when runnerMode is External")
		}
		if strings.TrimSpace(spec.EngineImage) == "" {
			return nil, errors.New("engineImage is required for forge-verifier when runnerMode is External")
		}
	default:
		return nil, fmt.Errorf("unsupported runnerMode %q", runnerMode)
	}

	deadlineSeconds := DefaultSandboxTimeoutSeconds
	if spec.SandboxSpec.ActiveDeadlineSeconds > 0 {
		deadlineSeconds = spec.SandboxSpec.ActiveDeadlineSeconds
	}
	outputLimit := defaultOutputSizeLimitQty.DeepCopy()
	if spec.SandboxSpec.OutputSizeLimit != nil && spec.SandboxSpec.OutputSizeLimit.Sign() > 0 {
		outputLimit = spec.SandboxSpec.OutputSizeLimit.DeepCopy()
	}
	holdDuration := spec.HoldMaxDuration
	if holdDuration <= 0 {
		holdDuration = DefaultHoldMaxDuration
	}

	if spec.Thresholds != nil {
		if err := ValidateThresholds(*spec.Thresholds); err != nil {
			return nil, fmt.Errorf("invalid verification thresholds: %w", err)
		}
	}

	nodeSelector := make(map[string]string, len(spec.Hardware.SelectorLabels)+len(spec.ExtraNodeSelector))
	maps.Copy(nodeSelector, spec.ExtraNodeSelector)
	maps.Copy(nodeSelector, spec.Hardware.SelectorLabels)
	if len(nodeSelector) == 0 {
		return nil, errors.New("hardware selector labels are required to pin ForgeSandbox to the probed GPU architecture")
	}

	gpuResources, err := buildSandboxContainerResources(spec)
	if err != nil {
		return nil, err
	}
	reportPath := path.Join(VerifiedMountPath, VerificationReportFileName)

	volumes := []core.Volume{
		{
			Name: InputVolumeName,
			VolumeSource: core.VolumeSource{
				ConfigMap: &core.ConfigMapVolumeSource{
					LocalObjectReference: core.LocalObjectReference{Name: spec.InputConfigMapName},
					Items:                slices.Clone(spec.InputConfigMapItems),
				},
			},
		},
		{
			Name: OutputVolumeName,
			VolumeSource: core.VolumeSource{
				EmptyDir: &core.EmptyDirVolumeSource{
					SizeLimit: &outputLimit,
				},
			},
		},
		{
			Name: VerifiedVolumeName,
			VolumeSource: core.VolumeSource{
				EmptyDir: &core.EmptyDirVolumeSource{
					SizeLimit: ptr.To(verifiedSizeLimitQty.DeepCopy()),
				},
			},
		},
	}

	var initContainers []core.Container
	if runnerMode == workercore.ModelDeploymentForgerRunnerModeMock {
		initContainers = []core.Container{
			{
				Name:            RunnerContainerName,
				Image:           operatorImage,
				Command:         []string{"gpustack-operator"},
				Args:            []string{"model-forger", "mock", "--input-dir", InputMountPath, "--output-dir", OutputMountPath},
				Resources:       *gpuResources.DeepCopy(),
				SecurityContext: restrictedContainerSecurityContext(),
				VolumeMounts: []core.VolumeMount{
					{Name: InputVolumeName, MountPath: InputMountPath, ReadOnly: true},
					{Name: OutputVolumeName, MountPath: OutputMountPath},
				},
			},
			{
				Name:            VerifierContainerName,
				Image:           operatorImage,
				Command:         []string{"gpustack-operator"},
				Args:            append([]string{"model-forger", "verify"}, verifierArgs(reportPath, spec.Thresholds)...),
				Resources:       *gpuResources.DeepCopy(),
				SecurityContext: restrictedContainerSecurityContext(),
				VolumeMounts: []core.VolumeMount{
					{Name: InputVolumeName, MountPath: InputMountPath, ReadOnly: true},
					{Name: OutputVolumeName, MountPath: OutputMountPath, ReadOnly: true},
					{Name: VerifiedVolumeName, MountPath: VerifiedMountPath},
				},
			},
		}
	} else {
		volumes = append(volumes, core.Volume{
			Name: SentinelVolumeName,
			VolumeSource: core.VolumeSource{
				EmptyDir: &core.EmptyDirVolumeSource{
					SizeLimit: ptr.To(sentinelSizeLimitQty.DeepCopy()),
				},
			},
		})
		runner := spec.SandboxSpec.Runner
		runnerContainer := core.Container{
			Name:            RunnerContainerName,
			Image:           strings.TrimSpace(runner.Image),
			ImagePullPolicy: runner.ImagePullPolicy,
			Env:             buildSandboxRunnerEnv(runner.Env),
			Resources:       *gpuResources.DeepCopy(),
			SecurityContext: restrictedContainerSecurityContext(),
			VolumeMounts: []core.VolumeMount{
				{Name: InputVolumeName, MountPath: InputMountPath, ReadOnly: true},
				{Name: OutputVolumeName, MountPath: OutputMountPath},
			},
		}
		if len(runner.Command) > 0 {
			runnerContainer.Command = slices.Clone(runner.Command)
		}
		if len(runner.Args) > 0 {
			runnerContainer.Args = slices.Clone(runner.Args)
		}
		if len(runner.EnvFrom) > 0 {
			runnerContainer.EnvFrom = make([]core.EnvFromSource, len(runner.EnvFrom))
			for i := range runner.EnvFrom {
				runner.EnvFrom[i].DeepCopyInto(&runnerContainer.EnvFrom[i])
			}
		}

		initContainers = []core.Container{
			runnerContainer,
			{
				Name:            StageSentinelContainerName,
				Image:           operatorImage,
				Command:         []string{"cp", "-R", OperatorSentinelSourceDir + "/.", SentinelMountPath + "/"},
				SecurityContext: restrictedContainerSecurityContext(),
				VolumeMounts: []core.VolumeMount{
					{Name: SentinelVolumeName, MountPath: SentinelMountPath},
				},
			},
			{
				Name:    VerifierContainerName,
				Image:   strings.TrimSpace(spec.EngineImage),
				Command: []string{"python3", "-m", "gpustack_engine_sentinel.forge_verifier"},
				Args:    verifierArgs(reportPath, spec.Thresholds),
				Env: []core.EnvVar{
					{Name: envPythonPath, Value: SentinelMountPath},
					{Name: envEngineSentinelEnable, Value: "0"},
					{Name: "HOME", Value: "/tmp"},
					{Name: "TMPDIR", Value: "/tmp"},
					{Name: "TRITON_CACHE_DIR", Value: "/tmp/triton"},
					{Name: "TORCHINDUCTOR_CACHE_DIR", Value: "/tmp/inductor"},
					{Name: "XDG_CACHE_HOME", Value: "/tmp/cache"},
				},
				Resources:       *gpuResources.DeepCopy(),
				SecurityContext: restrictedContainerSecurityContext(),
				VolumeMounts: []core.VolumeMount{
					{Name: InputVolumeName, MountPath: InputMountPath, ReadOnly: true},
					{Name: OutputVolumeName, MountPath: OutputMountPath, ReadOnly: true},
					{Name: VerifiedVolumeName, MountPath: VerifiedMountPath},
					{Name: SentinelVolumeName, MountPath: SentinelMountPath, ReadOnly: true},
				},
			},
		}
	}

	holdContainer := core.Container{
		Name:            HoldContainerName,
		Image:           operatorImage,
		Command:         []string{"gpustack-operator"},
		Args:            []string{"model-forger", "hold", "--max-duration", holdDuration.String()},
		SecurityContext: restrictedContainerSecurityContext(),
	}

	labels := sandboxCommonLabels(spec.Forger.Name, spec.RoleName, spec.Generation)
	labels[kueuectrlconst.QueueLabel] = entrance

	pod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Name:            spec.Name,
			Namespace:       spec.Forger.Namespace,
			Labels:          labels,
			OwnerReferences: []meta.OwnerReference{forgerControllerOwnerRef(spec.Forger)},
		},
		Spec: core.PodSpec{
			RestartPolicy:                core.RestartPolicyNever,
			ActiveDeadlineSeconds:        ptr.To(deadlineSeconds),
			AutomountServiceAccountToken: ptr.To(false),
			PriorityClassName:            strings.TrimSpace(spec.SandboxSpec.PriorityClassName),
			NodeSelector:                 nodeSelector,
			ImagePullSecrets:             mergeImagePullSecrets(spec.ImagePullSecrets, spec.SandboxSpec.ImagePullSecrets),
			SecurityContext:              restrictedPodSecurityContext(),
			InitContainers:               initContainers,
			Containers:                   []core.Container{holdContainer},
			Volumes:                      volumes,
		},
	}
	return pod, nil
}

func buildSandboxRunnerEnv(userEnv []workercore.ModelDeploymentForgerSandboxEnvVar) []core.EnvVar {
	env := make([]core.EnvVar, 0, 2+len(userEnv))
	env = append(env,
		core.EnvVar{Name: envForgeInputDir, Value: InputMountPath},
		core.EnvVar{Name: envForgeOutputDir, Value: OutputMountPath},
	)
	for i := range userEnv {
		ev := &userEnv[i]
		name := strings.TrimSpace(ev.Name)
		if name == "" || strings.HasPrefix(strings.ToUpper(name), reservedForgeEnvPrefix) {
			continue
		}
		out := core.EnvVar{
			Name:  name,
			Value: ev.Value,
		}
		if ev.ValueFrom != nil {
			var src core.EnvVarSource
			if ev.ValueFrom.ConfigMapKeyRef != nil {
				src.ConfigMapKeyRef = ev.ValueFrom.ConfigMapKeyRef.DeepCopy()
			}
			if ev.ValueFrom.SecretKeyRef != nil {
				src.SecretKeyRef = ev.ValueFrom.SecretKeyRef.DeepCopy()
			}
			if src.ConfigMapKeyRef != nil || src.SecretKeyRef != nil {
				out.ValueFrom = &src
			}
		}
		env = append(env, out)
	}
	return env
}

func mergeImagePullSecrets(
	roleSecrets, sandboxSecrets []core.LocalObjectReference,
) []core.LocalObjectReference {
	total := len(roleSecrets) + len(sandboxSecrets)
	if total == 0 {
		return nil
	}
	seen := make(map[string]struct{}, total)
	merged := make([]core.LocalObjectReference, 0, total)
	for _, list := range [][]core.LocalObjectReference{roleSecrets, sandboxSecrets} {
		for _, ref := range list {
			name := strings.TrimSpace(ref.Name)
			if name == "" {
				continue
			}
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			merged = append(merged, core.LocalObjectReference{Name: name})
		}
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

func verifierArgs(reportPath string, thresholds *VerificationThresholds) []string {
	args := []string{
		"--input-dir", InputMountPath,
		"--output-dir", OutputMountPath,
		"--report-path", reportPath,
	}
	if thresholds != nil {
		args = append(args,
			"--min-speedup-ratio", strconv.FormatFloat(thresholds.MinSpeedupRatio, 'f', -1, 64),
			"--atol", strconv.FormatFloat(thresholds.Atol, 'g', -1, 64),
			"--rtol", strconv.FormatFloat(thresholds.Rtol, 'g', -1, 64),
		)
	}
	return args
}

func resolveSandboxManufacturer(spec SandboxPodSpec) (string, error) {
	var detailManu string
	if spec.AcceleratorType != nil {
		detailManu = strings.TrimSpace(spec.AcceleratorType.Status.Detail.Manufacturer)
	}
	rawManu := detailManu
	if rawManu == "" {
		rawManu = strings.TrimSpace(spec.Manufacturer)
	}
	if rawManu == "" {
		rawManu = strings.TrimSpace(spec.Hardware.Manufacturer)
	}
	normManu, err := modelkernelkey.NormalizeManufacturer(rawManu)
	if err != nil {
		return "", fmt.Errorf("resolve accelerator manufacturer: %w", err)
	}
	if detailManu != "" && nodefeature.GetAcceleratableResourceName(detailManu, workercore.DeviceAllocationModeExclusive) != "" {
		return detailManu, nil
	}
	return normManu, nil
}

func buildSandboxContainerResources(spec SandboxPodSpec) (core.ResourceRequirements, error) {
	rr := core.ResourceRequirements{
		Requests: core.ResourceList{},
		Limits:   core.ResourceList{},
	}
	res := spec.SandboxSpec.Resources
	oneCard := *resource.NewQuantity(1, resource.DecimalSI)

	var (
		partProfile string
		memPct      int32
		coresPct    int32
	)
	if res != nil {
		partProfile = res.AcceleratorPartitionedProfile
		memPct = res.AcceleratorSlicedMemoryPercentage
		coresPct = res.AcceleratorSlicedCoresPercentage
	}

	if memPct < 0 || memPct > 100 {
		return rr, fmt.Errorf("acceleratorSlicedMemoryPercentage %d must be between 0 and 100", memPct)
	}
	if coresPct < 0 || coresPct > 100 {
		return rr, fmt.Errorf("acceleratorSlicedCoresPercentage %d must be between 0 and 100", coresPct)
	}

	switch {
	case partProfile != "":
		if memPct > 0 || coresPct > 0 {
			return rr, errors.New(
				"acceleratorPartitionedProfile is mutually exclusive with " +
					"acceleratorSlicedMemoryPercentage and acceleratorSlicedCoresPercentage",
			)
		}
		if strings.TrimSpace(partProfile) != partProfile {
			return rr, fmt.Errorf("invalid acceleratorPartitionedProfile %q: must not contain surrounding whitespace", partProfile)
		}
		manu, err := resolveSandboxManufacturer(spec)
		if err != nil {
			return rr, err
		}
		partCardResName := nodefeature.GetAcceleratableResourceName(manu, workercore.DeviceAllocationModePartitioned)
		partProfileResName := nodefeature.GetAcceleratablePartitionedProfileResourceName(manu, partProfile)
		if partCardResName == "" || partProfileResName == "" {
			return rr, fmt.Errorf("manufacturer %q or partition profile %q does not yield valid partitioned resource names", manu, partProfile)
		}
		rr.Requests[partCardResName] = oneCard.DeepCopy()
		rr.Limits[partCardResName] = oneCard.DeepCopy()
		rr.Requests[partProfileResName] = oneCard.DeepCopy()
		rr.Limits[partProfileResName] = oneCard.DeepCopy()

	case memPct > 0:
		if spec.AcceleratorType != nil && !spec.AcceleratorType.Status.Detail.IsLogicallySliceable() {
			return rr, fmt.Errorf("accelerator type %q does not support logical slicing", spec.AcceleratorType.Name)
		}
		manu, err := resolveSandboxManufacturer(spec)
		if err != nil {
			return rr, err
		}
		if nodefeature.GetAcceleratableResourceName(manu, workercore.DeviceAllocationModeExclusive) == "" {
			return rr, fmt.Errorf("manufacturer %q yields no accelerator resource name", manu)
		}
		slicedResName := nodefeature.GetAcceleratableResourceName(manu, workercore.DeviceAllocationModeSliced)
		memResName := nodefeature.GetAcceleratableSlicedMemoryPercentageResourceName(manu)
		coresResName := nodefeature.GetAcceleratableSlicedCoresPercentageResourceName(manu)
		if slicedResName == "" || memResName == "" || coresResName == "" {
			return rr, fmt.Errorf("manufacturer %q yields empty sliced accelerator resource names", manu)
		}
		memQ := *resource.NewQuantity(int64(memPct), resource.DecimalSI)
		coresQ := *resource.NewQuantity(int64(coresPct), resource.DecimalSI)
		rr.Requests[slicedResName] = oneCard.DeepCopy()
		rr.Limits[slicedResName] = oneCard.DeepCopy()
		rr.Requests[memResName] = memQ.DeepCopy()
		rr.Limits[memResName] = memQ.DeepCopy()
		rr.Requests[coresResName] = coresQ.DeepCopy()
		rr.Limits[coresResName] = coresQ.DeepCopy()

	default:
		if coresPct > 0 {
			return rr, errors.New("acceleratorSlicedCoresPercentage requires acceleratorSlicedMemoryPercentage > 0")
		}
		accelResName := spec.AcceleratorResourceName
		if accelResName == "" {
			manu, err := resolveSandboxManufacturer(spec)
			if err != nil {
				return rr, err
			}
			accelResName = nodefeature.GetAcceleratableResourceName(manu, workercore.DeviceAllocationModeExclusive)
			if accelResName == "" {
				return rr, fmt.Errorf("manufacturer %q yields no exclusive accelerator resource name", manu)
			}
		}
		rr.Requests[accelResName] = oneCard.DeepCopy()
		rr.Limits[accelResName] = oneCard.DeepCopy()
	}

	if res != nil {
		if res.CPU != nil && res.CPU.Sign() > 0 {
			rr.Requests[core.ResourceCPU] = res.CPU.DeepCopy()
			rr.Limits[core.ResourceCPU] = res.CPU.DeepCopy()
		}
		if res.RAM != nil && res.RAM.Sign() > 0 {
			rr.Requests[core.ResourceMemory] = res.RAM.DeepCopy()
			rr.Limits[core.ResourceMemory] = res.RAM.DeepCopy()
		}
		if res.LocalStorage != nil && res.LocalStorage.Sign() > 0 {
			rr.Requests[core.ResourceEphemeralStorage] = res.LocalStorage.DeepCopy()
			rr.Limits[core.ResourceEphemeralStorage] = res.LocalStorage.DeepCopy()
		}
	}

	return rr, nil
}

func restrictedPodSecurityContext() *core.PodSecurityContext {
	return &core.PodSecurityContext{
		RunAsNonRoot: ptr.To(true),
		RunAsUser:    ptr.To[int64](65532),
		RunAsGroup:   ptr.To[int64](65532),
		FSGroup:      ptr.To[int64](65532),
		SeccompProfile: &core.SeccompProfile{
			Type: core.SeccompProfileTypeRuntimeDefault,
		},
	}
}

func restrictedContainerSecurityContext() *core.SecurityContext {
	return &core.SecurityContext{
		AllowPrivilegeEscalation: ptr.To(false),
		RunAsNonRoot:             ptr.To(true),
		RunAsUser:                ptr.To[int64](65532),
		RunAsGroup:               ptr.To[int64](65532),
		Capabilities: &core.Capabilities{
			Drop: []core.Capability{"ALL"},
		},
		SeccompProfile: &core.SeccompProfile{
			Type: core.SeccompProfileTypeRuntimeDefault,
		},
	}
}

func sandboxCommonLabels(forgerName, roleName string, generation int64) map[string]string {
	return map[string]string{
		SandboxLabelName:       sandboxLabelValueName,
		SandboxLabelInstance:   SafeLabelValue(forgerName),
		SandboxLabelComponent:  sandboxLabelValueComponent,
		SandboxLabelRole:       SafeLabelValue(roleName),
		SandboxLabelGeneration: strconv.FormatInt(generation, 10),
	}
}

// SafeLabelValue returns a valid Kubernetes label value (<= 63 chars) derived from raw,
// preserving valid label values verbatim and hashing longer or non-label-safe strings.
func SafeLabelValue(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	if len(trimmed) <= kvalidation.DNS1123LabelMaxLength && len(kvalidation.IsValidLabelValue(trimmed)) == 0 {
		return trimmed
	}
	sum := sha256.Sum256([]byte(trimmed))
	suffix := hex.EncodeToString(sum[:])[:8]

	prefix := safeConfigMapKeyCharRe.ReplaceAllString(trimmed, "-")
	prefix = strings.TrimLeft(prefix, "._-")
	maxPrefixLen := kvalidation.DNS1123LabelMaxLength - 1 - len(suffix)
	if len(prefix) > maxPrefixLen {
		prefix = prefix[:maxPrefixLen]
	}
	prefix = strings.TrimRight(prefix, "._-")
	if prefix == "" {
		return suffix
	}
	return prefix + "-" + suffix
}

func forgerControllerOwnerRef(forger *workercore.ModelDeploymentForger) meta.OwnerReference {
	return meta.OwnerReference{
		APIVersion:         workercore.SchemeGroupVersion.String(),
		Kind:               "ModelDeploymentForger",
		Name:               forger.Name,
		UID:                forger.UID,
		Controller:         ptr.To(true),
		BlockOwnerDeletion: ptr.To(true),
	}
}

func sanitizeConfigMapSegment(raw string) string {
	cleaned := safeConfigMapKeyCharRe.ReplaceAllString(strings.TrimSpace(raw), "_")
	cleaned = strings.Trim(cleaned, "._-")
	if cleaned == "" {
		return "op"
	}
	if len(cleaned) > 120 {
		cleaned = cleaned[:120]
	}
	return cleaned
}

func marshalCompactJSON(v any) ([]byte, error) {
	return json.Marshal(v)
}
