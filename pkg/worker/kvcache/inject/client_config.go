// This file holds what every engine is told, and the artifact only the vLLM family takes: its config
// file. "The vLLM family" is two engines with two connector names and two readers closed at
// different key sets, which is why the values below are described per reader rather than per family.
//
// The values below are rendered for a Mooncake client that stores nothing of its own. That is a role
// rather than a size setting: a client with a local buffer and no global segment can Get and Put and
// contributes no memory, which is what an inference engine needs. Declaring the other shapes by
// accident is the failure this file is arranged to prevent, so the keys that declare the role are
// written together and never one at a time.
package inject

import (
	"encoding/json"
	"fmt"

	"gpustack.ai/gpustack/pkg/systemname"
)

const (
	// MetadataServer is the value every participant writes for the metadata plane. It is a constant
	// rather than an address: the plane is peer-to-peer, so there is no server to name, and a value
	// that looks like an endpoint would send a reader hunting for a component that does not exist.
	MetadataServer = "P2PHANDSHAKE"

	// GlobalSegmentSize is the host memory an engine container contributes to the pool: none. The
	// pool's capacity comes from the backend's declared members, whose Pods request that memory in
	// their own `resources`. An engine container contributing memory would change its real footprint
	// without appearing in any resource request, and this package's caller never mutates `resources`.
	GlobalSegmentSize = 0

	// LocalBufferSize is the client-side staging buffer, at the value the store's own reference uses.
	// It is transfer-layer staging rather than a tenant allowance, which is why it is a constant here
	// and reads from no API field.
	//
	// It is written rather than omitted because the layers disagree by orders of magnitude: the file
	// readers default to 4 GiB on vLLM (`worker.py:75`) and 1 GiB on vLLM-Ascend
	// (`mooncake_backend.py:19`), while the client's own default is 16 MiB - and the file reader's
	// value is the one that wins. An absent key holds GiB of host memory per container, inside a
	// limit its owner sized for a model. Which engine it is changes the number and not the outcome.
	LocalBufferSize = 128 * 1024 * 1024

	// ModeStandaloneStore is the topology a pure client declares. vLLM raises at startup unless it
	// agrees with GlobalSegmentSize, in both directions, so the two are always written as a pair.
	ModeStandaloneStore = "standalone-store"

	// DeviceName is the RDMA device filter, empty on every path including RDMA. Empty means "use every
	// device found", which is the only value correct for every host in one pool: a device is named per
	// host, mlx5_0 on one and erdma_0 on the next. The documented string "auto-discovery" is not
	// special-cased anywhere in the client - it is parsed as a filter naming a device no host has.
	DeviceName = ""
)

// ClientConfigAnnotationKey carries the rendered client configuration on the Pod that consumes it, for
// engines that take a file. The file is a downwardAPI projection of this annotation, so the caller
// creates no ConfigMap, needs no RBAC for one, and leaves nothing to garbage-collect: the
// configuration's lifetime is exactly the Pod's.
//
// It is declared here rather than beside the annotations a caller READS, because this one is an output
// of rendering. A caller that refuses a user-supplied value for it reads the constant from here.
const ClientConfigAnnotationKey = "kvcache." + systemname.LabelPrefix + "client-config"

const (
	// ConfigVolumeName and ConfigMountPath are where the rendered file lands. The mount is read-only:
	// nothing in the container has any reason to write it, and a writable projection would let a
	// process edit the record of what was injected.
	ConfigVolumeName = "gpustack-kvcache-config"
	ConfigMountPath  = "/etc/gpustack/kvcache"

	// ConfigFileName is the file's name inside the mount, and ConfigFilePath the two joined - the
	// value the engine's own path variable is set to.
	ConfigFileName = "mooncake.json"
	ConfigFilePath = ConfigMountPath + "/" + ConfigFileName
)

// vllmClientConfig is vLLM's Mooncake configuration file, in that engine's own spelling.
//
// The struct is the schema, and being closed is the point: vLLM's reader takes eight named keys one by
// one with no passthrough (`worker.py:129-142`), so a key outside the set is not merely ignored, no
// code path ever sees it. Rendering through a struct rather than a map makes an unreadable key a
// compile error instead of a silent addition.
//
// TWO readers consume this one file, and they are closed at different sets: vLLM-Ascend's takes six
// of the same names and has no `mode` (`mooncake_backend.py:115-124`, v0.19.1rc1). We render `mode`
// anyway, and that is safe for a reason worth stating rather than assuming - vLLM does not pass
// `mode` to `store.setup()` either (`worker.py:1040-1048`); it only validates the pair against
// `global_segment_size` and logs it. The value that drives both engines is the size, and both read
// it. A key one reader ignores is only harmless when the reader that HAS it does not act on it.
//
// `enable_offload` is the one readable key deliberately absent: it selects a storage tier this
// injection does not configure, and its default is the value we would write.
//
// The names differ from the client's `setup()` signature and the file's names are the ones that count:
// `master_server_address` carries four letters the parameter does not, and the RDMA filter is
// `device_name` here against `rdma_devices` there.
type vllmClientConfig struct {
	MetadataServer      string `json:"metadata_server"`
	MasterServerAddress string `json:"master_server_address"`
	Protocol            string `json:"protocol"`
	DeviceName          string `json:"device_name"`
	Mode                string `json:"mode"`
	GlobalSegmentSize   int64  `json:"global_segment_size"`
	LocalBufferSize     int64  `json:"local_buffer_size"`
}

// There is deliberately no tenant key, and it stays absent for BOTH engines rendered onto this file
// even though they are now configured with different connectors - vllm with MooncakeStoreConnector,
// vllm-ascend with AscendStoreConnector (see vllmConnectorFor). Neither reader has a tenant: vLLM's
// takes eight named keys with no tenant among them (worker.py:126-142, v0.25.1), and vLLM-Ascend's
// takes six, also without one (mooncake_backend.py:115-124, v0.19.1rc1) - that release has no tenant
// anywhere, tests excluded.
//
// An earlier revision said the key "comes back with that connector", meaning AscendStoreConnector.
// That connector is now what this project renders and the key did not come back, because the
// prediction read a tenant capability off a connector name. The two are independent.

// renderVLLMClientConfig builds the file's content for a resolved connection.
//
// Every field is written, none defaulted by omission. The engine's default for an absent key is not
// something a reader should have to know to predict the container's behavior, and in two cases -
// `global_segment_size` and `local_buffer_size` - the default is GiB of host memory nobody asked for
// (4 on vLLM, 1 on vLLM-Ascend). Writing every field is also what keeps that difference from
// mattering: two engines with different defaults produce the same container when nothing defaults.
func renderVLLMClientConfig(conn Connection) ([]byte, error) {
	cfg := vllmClientConfig{
		MetadataServer:      MetadataServer,
		MasterServerAddress: conn.MasterAddress,
		Protocol:            conn.Protocol,
		DeviceName:          DeviceName,
		Mode:                ModeStandaloneStore,
		GlobalSegmentSize:   GlobalSegmentSize,
		LocalBufferSize:     LocalBufferSize,
	}

	out, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("marshal the vllm client configuration: %w", err)
	}
	return out, nil
}
