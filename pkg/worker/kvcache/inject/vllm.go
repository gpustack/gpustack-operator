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
	vllmKVEventsConfigArg = "--kv-events-config"
	// VLLMKVEventsPort is the ZMQ publisher port consumed by managed routers.
	VLLMKVEventsPort int32 = 5557
	// VLLMKVEventsReplayPort is the publisher's replay port.
	VLLMKVEventsReplayPort int32 = 5558
	// VLLMKVEventsTopic is the topic both publisher and subscriber use.
	VLLMKVEventsTopic = "kv@"
	// VLLMMooncakeBootstrapPort is where a prefiller exposes Mooncake's transfer handshake.
	VLLMMooncakeBootstrapPort int32 = 8998

	// vllmKVTransferProtocol is the transport the prefill-to-decode leg is told to use when
	// the caller declares none, and it is deliberately NOT resolved from the backend.
	//
	// KVCacheBackend.spec.transport defines the data plane the store MEMBERS run. This leg is
	// engine to engine and never traverses the store, so the two planes have no business sharing
	// one value -- yet they did: a pair with no store always rendered tcp even on fabric
	// hardware, and a pair with one inherited the members' transport, an RDMA pool telling
	// engine Pods to run a fabric this operator gives them no access to. The backend-level field
	// is also set to become an inherited default once member groups can override it, which would
	// leave this leg reading a value no group necessarily uses.
	//
	// No source can DISCOVER the right value: the accepted set is a property of the mooncake
	// build inside the engine's own image, which this operator neither ships nor can inspect.
	// The value is therefore DECLARED, and the declarer is the ModelDeployment's
	// spec.kvTransfer.protocol. This constant is the default when that field is unset:
	// "tcp" is the one answer honest from here -- the transport every mooncake build carries,
	// and what a store-less pair has always rendered. A pair whose engines can speak a fabric
	// protocol says so through the API; the gating rule below binds the declared value exactly
	// as it binds this default.
	vllmKVTransferProtocol = "tcp"

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

	// vllmAscendTransferConnector is the name vLLM-Ascend registers for its point-to-point
	// connector (`vllm_ascend/distributed/kv_transfer/__init__.py:29-31`, read at v0.23.0), loaded
	// through the project's `ascend_kv_connector` entry point rather than named by path.
	//
	// It is NOT the connector vLLM proper registers under the same purpose: the two speak different
	// handshake vocabularies over kv_transfer_params -- proper's MooncakeConnector wants a
	// transfer_id and a bootstrap address, while this one wants the prefiller's host, port, block
	// ids and engine id, which is why a router that proxies between them must match the engine.
	vllmAscendTransferConnector = "MooncakeConnectorV1"

	// vllmAscendDriverPath is the host driver tree the Ascend transfer leg reads each NPU's NIC
	// address through: hccn_tool ships under it, while the engine image carries the driver
	// libraries but not the tool. Host source and container mount point are one path, which is
	// where the image's own driver stubs sit.
	vllmAscendDriverPath = "/usr/local/Ascend/driver"

	// vllmAscendDriverVolumeName names the projection of that tree on a Pod.
	vllmAscendDriverVolumeName = "gpustack-ascend-driver"
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
	KVConnector            string                    `json:"kv_connector"`
	KVRole                 string                    `json:"kv_role"`
	KVPort                 int32                     `json:"kv_port,omitempty"`
	KVConnectorExtraConfig *vllmConnectorExtraConfig `json:"kv_connector_extra_config,omitempty"`
}

type vllmConnectorExtraConfig struct {
	Connectors       []vllmTransferConfig `json:"connectors,omitempty"`
	MooncakeProtocol string               `json:"mooncake_protocol,omitempty"`
	Prefill          *vllmRoleParallelism `json:"prefill,omitempty"`
	Decode           *vllmRoleParallelism `json:"decode,omitempty"`
}

// vllmRoleParallelism is the per-half parallel shape vLLM-Ascend's point-to-point connector
// ASSERTS on at worker start (`vllm_ascend/distributed/kv_transfer/kv_p2p/mooncake_connector.py:2066-2084`,
// read at v0.23.0): a missing tp_size or dp_size under either key crashes the engine, and the
// prefill value must be at least the decode one. The operator renders no parallelism flag, so an
// engine it configured runs one device per Pod and the honest value is one; a role widened by
// hand through ExtraArgs makes this wrong, which is a stated limit of the Ascend leg.
type vllmRoleParallelism struct {
	TPSize int `json:"tp_size"`
	DPSize int `json:"dp_size"`
}

type vllmKVEventsConfig struct {
	EnableKVCacheEvents bool   `json:"enable_kv_cache_events"`
	Publisher           string `json:"publisher"`
	Endpoint            string `json:"endpoint"`
	ReplayEndpoint      string `json:"replay_endpoint"`
	BufferSteps         int    `json:"buffer_steps"`
	HWM                 int    `json:"hwm"`
	MaxQueueSize        int    `json:"max_queue_size"`
	Topic               string `json:"topic"`
}

func renderVLLM(in Input) (*Result, error) {
	kvRole, err := vllmKVRole(in.Role)
	if err != nil {
		return nil, err
	}

	// Reads the address alone, while Render's gate spells the same question as "an address OR a
	// transport". The two are EQUIVALENT ON EVERY INPUT THAT REACHES HERE, and only because that gate
	// refuses a connection carrying one without the other - so a Connection arriving here has both
	// fields or neither. TestRender_RefusesAHalfConnection pins that, because the equivalence is a
	// property of the caller rather than of this line, and nothing here would notice it changing.
	hasStore := in.Connection.MasterAddress != ""

	// The connector configuration is a JSON document on the command line, marshaled from a type so an
	// unreadable key is a compile error. Key order is not part of the contract - JSON defines none -
	// so anything comparing this must decode it rather than match the string.
	var transferConfigValue *vllmTransferConfig
	// The per-vendor leg arms below also fill what the leg itself needs mounted on the Pod.
	var legVolumes []core.Volume
	var legVolumeMounts []core.VolumeMount
	if hasStore {
		connector, err := vllmConnectorFor(in.Engine)
		if err != nil {
			return nil, err
		}
		transferConfigValue = &vllmTransferConfig{KVConnector: connector, KVRole: kvRole}
	}
	if in.KVTransfer {
		native := in.Engine == EngineVLLM
		if (!native && in.Engine != EngineVLLMAscend) || (in.Role != RolePrefill && in.Role != RoleDecode) {
			return nil, newRefusal(ReasonRoleUnsupported,
				"point-to-point transfer requires a native vLLM prefill or decode role")
		}
		var direct vllmTransferConfig
		if native {
			// This value is NOT gated, on purpose. The accepted set is a property of the mooncake
			// build inside the engine's own image, which this operator neither ships nor can
			// inspect: a HIP-compiled build makes "hip" a working point-to-point transport, and
			// refusing it here would hard-code one image's compile set onto another image's
			// connector. checkTransport documents the same rule from the other side -- an
			// unmeasured pair is let through, because a refusal on a fact nobody read turns a
			// working engine into a broken one. A mismatch therefore still raises at startup, in
			// the container that owns the fact. The rule binds the declared value and the default
			// alike.
			protocol := vllmKVTransferProtocol
			if in.KVTransferProtocol != "" {
				protocol = in.KVTransferProtocol
			}
			direct = vllmTransferConfig{
				KVConnector: "MooncakeConnector", KVRole: kvRole,
				KVConnectorExtraConfig: &vllmConnectorExtraConfig{MooncakeProtocol: protocol},
			}
		} else {
			// The Ascend leg renders NO protocol key: its transfer engine is initialized with
			// the literal "ascend" (`vllm_ascend/distributed/kv_transfer/utils/mooncake_transfer_engine.py:26`,
			// read at v0.23.0), so spec.kvTransfer.protocol has no key to land in here and is
			// ignored rather than refused -- admission cannot know the pool's vendor, and a
			// refusal past admission is an error loop.
			//
			// kv_port is the one value both halves MUST agree on, and the agreement is structural:
			// the prefiller advertises its side channel as an OFFSET from the consumer's own
			// kv_port (`kv_p2p/mooncake_connector.py:1936-1945`), so two different values send
			// the decoder dialing ports nothing listens on. VLLMMooncakeBootstrapPort is that one
			// value; admission already reserves it on a routed prefiller of either vendor.
			direct = vllmTransferConfig{
				KVConnector: vllmAscendTransferConnector,
				KVRole:      kvRole,
				KVPort:      VLLMMooncakeBootstrapPort,
				KVConnectorExtraConfig: &vllmConnectorExtraConfig{
					Prefill: &vllmRoleParallelism{TPSize: 1, DPSize: 1},
					Decode:  &vllmRoleParallelism{TPSize: 1, DPSize: 1},
				},
			}
			// Both role containers mount the host driver tree, read-only. The leg's transport
			// builds Device RoCE endpoints and reads each NPU's NIC address through hccn_tool,
			// which ships with the driver -- the engine image carries the driver libraries but
			// not the tool, and with neither source reachable the engine dies at startup with
			// "Failed to get device ip from hccn.conf and hccn_tool". Reading /etc/hccn.conf
			// works too, but only on a host that keeps the file, while the tool answers from
			// the driver on every host that has one, so the tree is the one mount that covers
			// both. The driver belongs to the host on every Ascend containerization path --
			// the image supplies the toolkit -- so the mount shadows nothing a container ships
			// with. It renders only alongside the transfer leg: projecting host paths into a
			// tenant-adjacent workload has a far larger blast radius than a cluster-scoped
			// DaemonSet, so it follows the capability that needs it rather than the vendor.
			// The Directory type turns a host missing the driver into a volume setup error
			// naming the path, a deploy-time signal instead of a startup failure.
			hostPathDirectory := core.HostPathDirectory
			legVolumes = append(legVolumes, core.Volume{
				Name: vllmAscendDriverVolumeName,
				VolumeSource: core.VolumeSource{
					HostPath: &core.HostPathVolumeSource{Path: vllmAscendDriverPath, Type: &hostPathDirectory},
				},
			})
			legVolumeMounts = append(legVolumeMounts, core.VolumeMount{
				Name: vllmAscendDriverVolumeName, MountPath: vllmAscendDriverPath, ReadOnly: true,
			})
		}
		if !hasStore {
			// The decode arm renders only the role and the protocol: the bootstrap address is
			// expected to arrive per-request via kv_transfer_params. That is verified behavior
			// from a real-cluster run; the upstream per-request path has not been read.
			transferConfigValue = &direct
		} else {
			// The inner store connector's kv_consumer/kv_both roles are hardcoded: upstream
			// per-inner-connector kv_role semantics are unverified. A real-cluster run observed
			// a Prometheus-metrics registration assert naming MooncakeConnector on the kv_both
			// role, which recovered after one APIServer restart.
			storeRole := "kv_consumer"
			if in.Role == RolePrefill {
				storeRole = "kv_both"
			}
			transferConfigValue = &vllmTransferConfig{
				KVConnector: "MultiConnector",
				KVRole:      kvRole,
				KVConnectorExtraConfig: &vllmConnectorExtraConfig{Connectors: []vllmTransferConfig{
					direct, *transferConfigValue,
				}},
			}
			transferConfigValue.KVConnectorExtraConfig.Connectors[1].KVRole = storeRole
		}
	}

	result := &Result{
		KVTransfer: in.KVTransfer,
	}
	if hasStore {
		config, err := renderVLLMClientConfig(in.Connection, in.Domain)
		if err != nil {
			return nil, err
		}
		result.TenantInjected = in.Domain != ""
		result.Env = []core.EnvVar{{Name: vllmConfigPathEnv, Value: ConfigFilePath}}
		result.Volumes = []core.Volume{
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
		}
		result.VolumeMounts = []core.VolumeMount{
			{Name: ConfigVolumeName, MountPath: ConfigMountPath, ReadOnly: true},
		}
		result.PodAnnotations = map[string]string{
			ClientConfigAnnotationKey: string(config),
		}
	}
	// Appended AFTER the store block, which assigns both slices: a pool-backed pair needs its
	// client configuration volume and the leg's mounts side by side.
	result.Volumes = append(result.Volumes, legVolumes...)
	result.VolumeMounts = append(result.VolumeMounts, legVolumeMounts...)
	if transferConfigValue != nil {
		transferDoc, err := json.Marshal(transferConfigValue)
		if err != nil {
			return nil, fmt.Errorf("marshal the vLLM connector configuration: %w", err)
		}
		result.Args = append(result.Args, vllmTransferConfigArg, string(transferDoc))
	}
	if in.KVTransfer && in.Role == RolePrefill {
		// The port is declared on either vendor: it is where the prefiller's handshake listener
		// sits, and the admission reservation that keeps a user's own port off it is
		// vendor-blind. The variable is vLLM proper's alone -- its connector reads the bootstrap
		// port from the environment, while vLLM-Ascend's takes it from the kv_port in the
		// document above and would never look here.
		if in.Engine == EngineVLLM {
			result.Env = append(result.Env, core.EnvVar{
				Name: "VLLM_MOONCAKE_BOOTSTRAP_PORT", Value: fmt.Sprint(VLLMMooncakeBootstrapPort),
			})
		}
		result.Ports = append(result.Ports, core.ContainerPort{
			Name: "mc-bootstrap", Protocol: core.ProtocolTCP,
			ContainerPort: VLLMMooncakeBootstrapPort,
		})
	}
	if !in.PublishKVEvents {
		return result, nil
	}
	if in.KVEventsHost == "" {
		return nil, newRefusal(ReasonConnectionIncomplete,
			"the KV event publisher has no dialable host")
	}

	eventsDoc, err := json.Marshal(vllmKVEventsConfig{
		EnableKVCacheEvents: true,
		Publisher:           "zmq",
		Endpoint:            fmt.Sprintf("tcp://*:%d", VLLMKVEventsPort),
		ReplayEndpoint:      fmt.Sprintf("tcp://*:%d", VLLMKVEventsReplayPort),
		BufferSteps:         10_000,
		HWM:                 100_000,
		MaxQueueSize:        100_000,
		Topic:               VLLMKVEventsTopic,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal the vLLM KV event configuration: %w", err)
	}
	result.Args = append(result.Args, vllmKVEventsConfigArg, string(eventsDoc))
	result.Ports = append(result.Ports, KVEventsPorts()...)
	result.KVEvents = VLLMKVEvents(in.KVEventsHost)

	return result, nil
}
