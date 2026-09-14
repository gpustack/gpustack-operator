// This file renders for the vLLM family. Its vehicle is a file, because that engine has no other:
// `MooncakeStoreConfig.load_from_config()` reads MOONCAKE_CONFIG_PATH and RAISES when it is unset
// (`worker.py:144-151`), and the variable names a file rather than carrying configuration.
package inject

import (
	"encoding/json"
	"fmt"

	core "k8s.io/api/core/v1"
)

const (
	// vllmConfigPathEnv is the variable vLLM reads the file's location from. It names a path; the
	// configuration itself is never in the environment for this engine.
	vllmConfigPathEnv = "MOONCAKE_CONFIG_PATH"

	// vllmTransferConfigArg selects the connector and the role. There is no environment equivalent -
	// it is an EngineArgs field parsed off the command line - so this engine cannot be configured
	// without appending an argument.
	vllmTransferConfigArg = "--kv-transfer-config"

	// vllmStoreConnector is the name vLLM PROPER registers for the Mooncake store
	// (`kv_connector/factory.py:223-226`, read at v0.25.1).
	//
	// `create_connector` looks the name up in a registry and raises `ValueError: Unsupported
	// connector type` on a miss, so a name that engine does not know stops it at startup. That does
	// NOT make this constant engine-independent: it is one project's registry spelling, and which
	// spellings any other engine's factory resolves is upstream state this repository neither
	// controls nor observes. Reach it through vllmConnectorFor, which selects by engine, rather than
	// rendering it directly.
	vllmStoreConnector = "MooncakeStoreConnector"

	// vllmAscendStoreConnector is the name vLLM-Ascend registers for its own store
	// (`vllm_ascend/distributed/kv_transfer/__init__.py:39-43`, read at v0.19.1rc1). It registers
	// `MooncakeConnectorStoreV1` for the same class two entries above; either name resolves, and
	// this one is chosen for saying which project owns the class.
	//
	// The connector name does not imply anything about tenant support; that belongs to the engine
	// image and is not a selection criterion here.
	vllmAscendStoreConnector = "AscendStoreConnector"
)

// vllmConnectorFor returns the connector name the given engine's own factory can resolve.
//
// The vLLM family shares a renderer because it shares a VEHICLE - a file at MOONCAKE_CONFIG_PATH,
// read by a `MooncakeStoreConfig.from_file` on both sides whose common keys carry the same meaning.
// It does NOT share a connector registry. Each project registers its own spelling, and which
// spellings any one of them resolves is upstream state this repository neither controls nor
// observes, so the name is selected per engine and never assumed to travel between them.
//
// What a per-engine name does NOT buy: two roles on different manufacturers sharing a cache. The
// key each side stores under is assembled by that project's own store client, which nothing this
// renderer emits selects or aligns. When the two disagree the failure is SILENT - both engines
// start, both serve, and each side's lookups return nothing for the other's entries. There is no
// engine log to read, so a cross-manufacturer cache that does nothing is not evidence that this
// function picked a wrong name.
func vllmConnectorFor(engine Engine) (string, error) {
	switch engine {
	case EngineVLLM:
		return vllmStoreConnector, nil
	case EngineVLLMAscend:
		return vllmAscendStoreConnector, nil
	default:
		// Unreachable: injectPod dispatches only these two engines here. Returned rather than
		// defaulted to vLLM's name, because defaulting is what a new vLLM derivative would silently
		// inherit - and inheriting this particular value is the failure being fixed.
		return "", newRefusal(ReasonEngineUnknown,
			"engine %q has no vllm-family connector name", engine)
	}
}

// vllmKVRole maps a declared role to the connector's own vocabulary. An unset role is `kv_both`,
// which is what a shared cache with no prefill/decode split wants: the container reads and writes.
func vllmKVRole(role Role) (string, error) {
	switch role {
	case RoleNone:
		return "kv_both", nil
	case RolePrefill:
		return "kv_producer", nil
	case RoleDecode:
		return "kv_consumer", nil
	default:
		return "", newRefusal(ReasonRoleUnknown, "role %q has no vllm connector role", role)
	}
}

// renderVLLM produces everything a vLLM-family container needs: the argument that selects the
// connector, the variable naming the file, the projection carrying it, and the annotation the
// projection reads from.
// vllmTransferConfig is the --kv-transfer-config document. A struct rather than a map so an
// unreadable key is a compile error, matching the client-config type in this package.
type vllmTransferConfig struct {
	KVConnector string `json:"kv_connector"`
	KVRole      string `json:"kv_role"`
}

func renderVLLM(in Input) (*Result, error) {
	kvRole, err := vllmKVRole(in.Role)
	if err != nil {
		return nil, err
	}

	connector, err := vllmConnectorFor(in.Engine)
	if err != nil {
		return nil, err
	}

	tenantInjected := in.Domain != ""
	config, err := renderVLLMClientConfig(in.Connection, in.Domain)
	if err != nil {
		return nil, err
	}

	// The connector configuration is a JSON document on the command line, marshaled from a type so an
	// unreadable key is a compile error. Key order is not part of the contract - JSON defines none -
	// so anything comparing this must decode it rather than match the string.
	transferDoc, err := json.Marshal(vllmTransferConfig{
		KVConnector: connector,
		KVRole:      kvRole,
	})
	if err != nil {
		// UNREACHABLE: two strings. Returned rather than ignored because dropping it would mean
		// discarding an error, and never panicked because this runs on an admission path.
		return nil, fmt.Errorf("marshal the vLLM connector configuration: %w", err)
	}
	transferConfig := string(transferDoc)

	return &Result{
		TenantInjected: tenantInjected,
		Env: []core.EnvVar{
			{Name: vllmConfigPathEnv, Value: ConfigFilePath},
		},
		Args: []string{vllmTransferConfigArg, transferConfig},
		Volumes: []core.Volume{
			{
				Name: ConfigVolumeName,
				VolumeSource: core.VolumeSource{
					DownwardAPI: &core.DownwardAPIVolumeSource{
						Items: []core.DownwardAPIVolumeFile{
							{
								Path: ConfigFileName,
								FieldRef: &core.ObjectFieldSelector{
									FieldPath: fmt.Sprintf("metadata.annotations['%s']",
										ClientConfigAnnotationKey),
								},
							},
						},
					},
				},
			},
		},
		VolumeMounts: []core.VolumeMount{
			{Name: ConfigVolumeName, MountPath: ConfigMountPath, ReadOnly: true},
		},
		PodAnnotations: map[string]string{
			ClientConfigAnnotationKey: string(config),
		},
	}, nil
}
