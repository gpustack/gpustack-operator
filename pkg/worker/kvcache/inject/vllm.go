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
	// A name the factory cannot resolve is a startup failure rather than a silent one, since
	// `create_connector` looks it up in a registry and raises `ValueError: Unsupported connector
	// type` on a miss. That cuts BOTH ways and the second way is the one that was missed: a name
	// correct for one engine is a misspelling for another, and this constant is not
	// engine-independent. It is resolvable only where vLLM's own registry is the one being
	// searched - see vllmConnectorFor.
	vllmStoreConnector = "MooncakeStoreConnector"

	// vllmAscendStoreConnector is the name vLLM-Ascend registers for its own store
	// (`vllm_ascend/distributed/kv_transfer/__init__.py:39-43`, read at v0.19.1rc1). It registers
	// `MooncakeConnectorStoreV1` for the same class two entries above; either name resolves, and
	// this one is chosen for saying which project owns the class.
	//
	// What this does NOT mean: that selecting it forwards a tenant. vLLM-Ascend has no tenant
	// anywhere in that release (grep over `vllm_ascend/`, excluding tests: zero hits), so this
	// engine's row in engineTenantSupport stays false after the change. The connector name and the
	// tenant answer are independent, and reading one off the other is what put the wrong name here.
	vllmAscendStoreConnector = "AscendStoreConnector"
)

// vllmConnectorFor returns the connector name the given engine's own factory can resolve.
//
// The vLLM family shares a renderer because it shares a VEHICLE - a file at MOONCAKE_CONFIG_PATH,
// read by a `MooncakeStoreConfig.from_file` on both sides whose common keys carry the same meaning.
// It does NOT share a connector registry: vLLM-Ascend pins vLLM v0.19.1, a release whose factory has
// no `MooncakeStoreConnector` at all (`factory.py:207,209` there registers `MooncakeConnector` and
// nothing else). Rendering vLLM's name for it aborts the engine before it ever opens the file this
// renderer projects.
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

	// No tenant is written into this file, for either engine on it - see the note on the config
	// struct. This renderer now selects TWO different connectors, one per engine, and neither has a
	// tenant key to read; the file is the same either way.
	config, err := renderVLLMClientConfig(in.Connection)
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
		// Nothing on this path writes a tenant, so the action this reports is always "none". Derived
		// from the renderer's own emission rather than from the engine table, so it stays true if a
		// later revision does emit one.
		TenantInjected: false,
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
