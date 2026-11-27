package forger

import (
	"bytes"
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
	"k8s.io/utils/ptr"
	kueuectrlconst "sigs.k8s.io/kueue/pkg/controller/constants"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/utils/json"
	"gpustack.ai/gpustack/pkg/worker/kernelkey"
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
	// DefaultHoldMaxDuration is the default maximum hold window for gpustack-operator forge-hold.
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

	workloadsByDef := make(map[string][]Workload)
	for i := range spec.Workloads {
		wl := spec.Workloads[i]
		if err := ValidateWorkload(&wl); err != nil {
			return nil, nil, fmt.Errorf("workloads[%d]: %w", i, err)
		}
		defKey := strings.TrimSpace(wl.Definition)
		if defKey == "" {
			defKey = strings.TrimSpace(wl.LocalKernelKey)
		}
		if defKey == "" {
			defKey = "default"
		}
		safeName := sanitizeConfigMapSegment(defKey)
		workloadsByDef[safeName] = append(workloadsByDef[safeName], wl)
	}

	wlGroupNames := slices.Sorted(maps.Keys(workloadsByDef))
	for _, safeName := range wlGroupNames {
		var buf bytes.Buffer
		for _, wl := range workloadsByDef[safeName] {
			line, err := marshalCompactJSON(wl)
			if err != nil {
				return nil, nil, fmt.Errorf("encode workload %q: %w", wl.UUID, err)
			}
			buf.Write(line)
			buf.WriteByte('\n')
		}
		cmKey := configMapWorkloadPrefix + safeName + ".jsonl"
		data[cmKey] = buf.String()
		items = append(items, core.KeyToPath{
			Key:  cmKey,
			Path: "workloads/" + safeName + ".jsonl",
		})
	}

	totalBytes := 0
	for k, v := range data {
		totalBytes += len(k) + len(v)
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
	Manufacturer            string
	AcceleratorResourceName core.ResourceName
	Hardware                kernelkey.NodeHardware
	ExtraNodeSelector       map[string]string
	OperatorImage           string
	EngineImage             string
	SandboxSpec             workercore.ModelDeploymentForgerSandboxSpec
	Thresholds              *VerificationThresholds
	HoldMaxDuration         time.Duration
	ImagePullSecrets        []core.LocalObjectReference
}

// BuildSandboxPod builds a Kueue-admitted, restricted Pod Security compliant ForgeSandbox Pod with
// a single whole accelerator card, GPU architecture nodeSelector, emptyDir staging volumes, and
// zero hostPath volumes.
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
		// Mock mode runs built-in Go subcommands inside OperatorImage.
	case workercore.ModelDeploymentForgerRunnerModeExternal:
		if strings.TrimSpace(spec.SandboxSpec.RunnerImage) == "" {
			return nil, errors.New("sandbox.runnerImage is required when runnerMode is External")
		}
		if strings.TrimSpace(spec.EngineImage) == "" {
			return nil, errors.New("engineImage is required for forge-verifier when runnerMode is External")
		}
	default:
		return nil, fmt.Errorf("unsupported runnerMode %q", runnerMode)
	}

	accelResName := spec.AcceleratorResourceName
	if accelResName == "" {
		manufacturer := strings.TrimSpace(spec.Manufacturer)
		if manufacturer == "" {
			manufacturer = strings.TrimSpace(spec.Hardware.Manufacturer)
		}
		normManu, err := kernelkey.NormalizeManufacturer(manufacturer)
		if err != nil {
			return nil, fmt.Errorf("resolve accelerator manufacturer: %w", err)
		}
		accelResName = nodefeature.GetAcceleratableResourceName(normManu, workercore.DeviceAllocationModeExclusive)
		if accelResName == "" {
			return nil, fmt.Errorf("manufacturer %q yields no exclusive accelerator resource name", normManu)
		}
	}

	timeoutSeconds := DefaultSandboxTimeoutSeconds
	if spec.SandboxSpec.TimeoutSeconds > 0 {
		timeoutSeconds = int64(spec.SandboxSpec.TimeoutSeconds)
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

	gpuResources := buildSandboxContainerResources(accelResName, spec.SandboxSpec.Resources)
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
				Args:            []string{"forge-mock", "--input-dir", InputMountPath, "--output-dir", OutputMountPath},
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
				Args:            append([]string{"forge-verify"}, verifierArgs(reportPath, spec.Thresholds)...),
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
		initContainers = []core.Container{
			{
				Name:  RunnerContainerName,
				Image: strings.TrimSpace(spec.SandboxSpec.RunnerImage),
				Env: []core.EnvVar{
					{Name: envForgeInputDir, Value: InputMountPath},
					{Name: envForgeOutputDir, Value: OutputMountPath},
				},
				Resources:       *gpuResources.DeepCopy(),
				SecurityContext: restrictedContainerSecurityContext(),
				VolumeMounts: []core.VolumeMount{
					{Name: InputVolumeName, MountPath: InputMountPath, ReadOnly: true},
					{Name: OutputVolumeName, MountPath: OutputMountPath},
				},
			},
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
		Args:            []string{"forge-hold", "--max-duration", holdDuration.String()},
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
			ActiveDeadlineSeconds:        ptr.To(timeoutSeconds),
			AutomountServiceAccountToken: ptr.To(false),
			PriorityClassName:            strings.TrimSpace(spec.SandboxSpec.PriorityClassName),
			NodeSelector:                 nodeSelector,
			ImagePullSecrets:             slices.Clone(spec.ImagePullSecrets),
			SecurityContext:              restrictedPodSecurityContext(),
			InitContainers:               initContainers,
			Containers:                   []core.Container{holdContainer},
			Volumes:                      volumes,
		},
	}
	return pod, nil
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

func buildSandboxContainerResources(
	accelResName core.ResourceName, hostRes *workercore.ModelDeploymentForgerSandboxResources,
) core.ResourceRequirements {
	oneCard := *resource.NewQuantity(1, resource.DecimalSI)
	rr := core.ResourceRequirements{
		Requests: core.ResourceList{
			accelResName: oneCard.DeepCopy(),
		},
		Limits: core.ResourceList{
			accelResName: oneCard.DeepCopy(),
		},
	}
	if hostRes != nil {
		if hostRes.CPU != nil && hostRes.CPU.Sign() > 0 {
			rr.Requests[core.ResourceCPU] = hostRes.CPU.DeepCopy()
			rr.Limits[core.ResourceCPU] = hostRes.CPU.DeepCopy()
		}
		if hostRes.RAM != nil && hostRes.RAM.Sign() > 0 {
			rr.Requests[core.ResourceMemory] = hostRes.RAM.DeepCopy()
			rr.Limits[core.ResourceMemory] = hostRes.RAM.DeepCopy()
		}
	}
	return rr
}

func restrictedPodSecurityContext() *core.PodSecurityContext {
	return &core.PodSecurityContext{
		RunAsNonRoot: ptr.To(true),
		SeccompProfile: &core.SeccompProfile{
			Type: core.SeccompProfileTypeRuntimeDefault,
		},
	}
}

func restrictedContainerSecurityContext() *core.SecurityContext {
	return &core.SecurityContext{
		AllowPrivilegeEscalation: ptr.To(false),
		RunAsNonRoot:             ptr.To(true),
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
		SandboxLabelInstance:   forgerName,
		SandboxLabelComponent:  sandboxLabelValueComponent,
		SandboxLabelRole:       roleName,
		SandboxLabelGeneration: strconv.FormatInt(generation, 10),
	}
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
