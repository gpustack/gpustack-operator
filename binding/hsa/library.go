package hsa

import "C"
import (
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"gpustack.ai/gpustack/binding"
)

var home string

func init() {
	for _, env := range []string{"ROCM_HOME", "ROCM_PATH"} {
		home = os.Getenv(env)
		if home != "" {
			break
		}
	}
	if home == "" {
		home = "/opt/rocm"
	}
}

const (
	STATUS_ERROR_FUNCTION_NOT_FOUND = -99998
	STATUS_ERROR_LIBRARY_NOT_FOUND  = -99999
)

type HSA struct {
	so binding.Library
}

// New creates a new HSA library instance.
// It attempts to load the HSA library from the system and sets up the function pointers for the HSA API functions.
func New(opts ...binding.LibraryOption) *HSA {
	soPaths := []string{
		"libhsa-runtime64.so.1",
		"libhsa-runtime64.so",
	}
	{
		if s, err := os.Stat(home); err == nil && s.IsDir() {
			soPaths = append(soPaths,
				filepath.Join(home, "lib", "libhsa-runtime64.so.1"),
				filepath.Join(home, "lib", "libhsa-runtime64.so"),
			)
		}
	}

	so := binding.NewLibrary(soPaths, opts...)

	return &HSA{so: so}
}

func (l *HSA) Init() Return {
	if err := l.so.Load(); err != nil {
		return STATUS_ERROR_LIBRARY_NOT_FOUND
	}
	if l.so.Lookup("hsa_init") != nil {
		return STATUS_ERROR_FUNCTION_NOT_FOUND
	}
	return hsaInit()
}

func (l *HSA) ShutDown() Return {
	if l.so.Lookup("amdsmi_shut_down") != nil {
		return STATUS_ERROR_FUNCTION_NOT_FOUND
	}
	ret := hsaShutDown()
	if !ret.IsSuccess() {
		return ret
	}
	if err := l.so.Unload(); err != nil {
		return STATUS_ERROR_LIBRARY_NOT_FOUND
	}
	return ret
}

type (
	// AgentProperty represents the properties of an HSA agent (GPU).
	AgentProperty struct {
		BDF              string
		UUID             string
		ProductName      string
		Name             string
		ComputeUnitCount uint32
		AsicFamilyId     uint32
	}
	// Agents is a map of AgentUUID to AgentProperty,
	// representing the properties of all detected HSA agents (GPUs).
	Agents map[string]AgentProperty
)

func (l *HSA) GetAgents() Agents {
	agents := Agents{}

	if l.so.Lookup("hsa_agent_get_info") != nil {
		return agents
	}
	if l.so.Lookup("hsa_iterate_agents") != nil {
		return agents
	}

	_ = hsaIterateAgents(func(agent Agent, data unsafe.Pointer) Return {
		device, _ := hsaAgentGetInfoDevice(agent)
		if DeviceType(device) != DEVICE_TYPE_GPU {
			return STATUS_SUCCESS // Skip non-GPU agents
		}

		bdf, _ := hsaAgentGetBDF(agent)
		uuid, _ := hsaAgentGetInfoUUID(agent)
		productName, _ := hsaAgentGetInfoProduceName(agent)
		name, _ := hsaAgentGetInfoName(agent)
		computeUnitCount, _ := hsaAgentGetInfoComputeUnitCount(agent)
		asicFamilyId, _ := hsaAgentGetInfoAsicFamilyId(agent)

		key := bdf
		if key == "" {
			key = uuid
		}
		agents[key] = AgentProperty{
			BDF:              bdf,
			UUID:             uuid,
			ProductName:      productName,
			Name:             name,
			ComputeUnitCount: computeUnitCount,
			AsicFamilyId:     asicFamilyId,
		}
		return STATUS_SUCCESS
	})

	return agents
}

// IsSuccess returns true if the Return value indicates success.
func (r Return) IsSuccess() bool {
	return r == STATUS_SUCCESS
}

// String returns the string representation of a Return.
func (r Return) String() string {
	return r.Error()
}

// Error returns the string representation of a Return.
func (r Return) Error() string {
	return defaultErrorStringFunc(r)
}

var defaultErrorStringFunc = func(r Return) string {
	switch r {
	case STATUS_SUCCESS:
		return "SUCCESS"
	case STATUS_INFO_BREAK:
		return "INFO_BREAK"
	case STATUS_ERROR:
		return "ERROR"
	case STATUS_ERROR_INVALID_ARGUMENT:
		return "INVALID_ARGUMENT"
	case STATUS_ERROR_INVALID_QUEUE_CREATION:
		return "INVALID_QUEUE_CREATION"
	case STATUS_ERROR_INVALID_ALLOCATION:
		return "INVALID_ALLOCATION"
	case STATUS_ERROR_INVALID_AGENT:
		return "INVALID_AGENT"
	case STATUS_ERROR_INVALID_REGION:
		return "INVALID_REGION"
	case STATUS_ERROR_INVALID_SIGNAL:
		return "INVALID_SIGNAL"
	case STATUS_ERROR_INVALID_QUEUE:
		return "INVALID_QUEUE"
	case STATUS_ERROR_OUT_OF_RESOURCES:
		return "OUT_OF_RESOURCES"
	case STATUS_ERROR_INVALID_PACKET_FORMAT:
		return "INVALID_PACKET_FORMAT"
	case STATUS_ERROR_RESOURCE_FREE:
		return "RESOURCE_FREE"
	case STATUS_ERROR_NOT_INITIALIZED:
		return "NOT_INITIALIZED"
	case STATUS_ERROR_REFCOUNT_OVERFLOW:
		return "REFCOUNT_OVERFLOW"
	case STATUS_ERROR_INCOMPATIBLE_ARGUMENTS:
		return "INCOMPATIBLE_ARGUMENTS"
	case STATUS_ERROR_INVALID_INDEX:
		return "INVALID_INDEX"
	case STATUS_ERROR_INVALID_ISA:
		return "INVALID_ISA"
	case STATUS_ERROR_INVALID_ISA_NAME:
		return "INVALID_ISA_NAME"
	case STATUS_ERROR_INVALID_CODE_OBJECT:
		return "INVALID_CODE_OBJECT"
	case STATUS_ERROR_INVALID_EXECUTABLE:
		return "INVALID_EXECUTABLE"
	case STATUS_ERROR_FROZEN_EXECUTABLE:
		return "FROZEN_EXECUTABLE"
	case STATUS_ERROR_INVALID_SYMBOL_NAME:
		return "INVALID_SYMBOL_NAME"
	case STATUS_ERROR_VARIABLE_ALREADY_DEFINED:
		return "VARIABLE_ALREADY_DEFINED"
	case STATUS_ERROR_VARIABLE_UNDEFINED:
		return "VARIABLE_UNDEFINED"
	case STATUS_ERROR_EXCEPTION:
		return "EXCEPTION"
	case STATUS_ERROR_INVALID_CODE_SYMBOL:
		return "INVALID_CODE_SYMBOL"
	case STATUS_ERROR_INVALID_EXECUTABLE_SYMBOL:
		return "INVALID_EXECUTABLE_SYMBOL"
	case STATUS_ERROR_INVALID_FILE:
		return "INVALID_FILE"
	case STATUS_ERROR_INVALID_CODE_OBJECT_READER:
		return "INVALID_CODE_OBJECT_READER"
	case STATUS_ERROR_INVALID_CACHE:
		return "INVALID_CACHE"
	case STATUS_ERROR_INVALID_WAVEFRONT:
		return "INVALID_WAVEFRONT"
	case STATUS_ERROR_INVALID_SIGNAL_GROUP:
		return "INVALID_SIGNAL_GROUP"
	case STATUS_ERROR_INVALID_RUNTIME_STATE:
		return "INVALID_RUNTIME_STATE"
	case STATUS_ERROR_FATAL:
		return "FATAL"
	default:
		return fmt.Sprintf("unknown return value: %d", r)

	}
}

// AMDGPU_FAMILY_SI = 110  # Hainan, Oland, Verde, Pitcairn, Tahiti
// AMDGPU_FAMILY_CI = 120  # Bonaire, Hawaii
// AMDGPU_FAMILY_KV = 125  # Kaveri, Kabini, Mullins
// AMDGPU_FAMILY_VI = 130  # Iceland, Tonga
// AMDGPU_FAMILY_CZ = 135  # Carrizo, Stoney
// AMDGPU_FAMILY_AI = 141  # Vega10
// AMDGPU_FAMILY_RV = 142  # Raven
// AMDGPU_FAMILY_NV = 143  # Navi10
// AMDGPU_FAMILY_VGH = 144  # Van Gogh
// AMDGPU_FAMILY_GC_11_0_0 = 145  # GC 11.0.0
// AMDGPU_FAMILY_YC = 146  # Yellow Carp
// AMDGPU_FAMILY_GC_11_0_1 = 148  # GC 11.0.1
// AMDGPU_FAMILY_GC_10_3_6 = 149  # GC 10.3.6
// AMDGPU_FAMILY_GC_10_3_7 = 151  # GC 10.3.7
// AMDGPU_FAMILY_GC_11_5_0 = 150  # GC 11.5.0
// AMDGPU_FAMILY_GC_12_0_0 = 152  # GC 12.0.0
var familyIdMap = map[uint32]string{
	110: "Southern Islands",
	120: "Sea Islands",
	125: "Kaveri",
	130: "Volcanic Islands",
	135: "Carrizo",
	141: "Arctic Islands",
	142: "Raven",
	143: "Navi",
	144: "Van Gogh",
	145: "GC 11.0.0",
	146: "Yellow Carp",
	148: "GC 11.0.1",
	149: "GC 10.3.6",
	151: "GC 10.3.7",
	150: "GC 11.5.0",
	152: "GC 12.0.0",
}

func (in AgentProperty) Family() string {
	if v, ok := familyIdMap[in.AsicFamilyId]; ok {
		return v
	}
	return ""
}
