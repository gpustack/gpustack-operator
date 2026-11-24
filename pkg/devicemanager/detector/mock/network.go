package mock

import (
	"errors"

	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/devicemanager/mockspec"
)

// DetectInterfaces resolves mock network interfaces for nodeName from store.
// When mock interfaces or an interfacesError are configured, configured is true.
func DetectInterfaces(nodeName string, store *mockspec.Store) ([]device.Interface, bool, error) {
	if store == nil {
		return nil, false, nil
	}
	nodeSpec, err := resolveNodeSpec(nodeName, store)
	if err != nil {
		return nil, true, err
	}
	if !nodeSpec.HasInterfaces() {
		return nil, false, nil
	}
	if nodeSpec.InterfacesError != "" {
		return nil, true, errors.New(nodeSpec.InterfacesError)
	}
	return nodeSpec.Interfaces, true, nil
}

// DetectRDMADevices resolves mock host RDMA verbs devices for nodeName from store.
// When mock RDMA devices or an rdmaError are configured, configured is true.
func DetectRDMADevices(nodeName string, store *mockspec.Store) ([]device.RDMADevice, bool, error) {
	if store == nil {
		return nil, false, nil
	}
	nodeSpec, err := resolveNodeSpec(nodeName, store)
	if err != nil {
		return nil, true, err
	}
	if !nodeSpec.HasRDMADevices() {
		return nil, false, nil
	}
	if nodeSpec.RDMAError != "" {
		return nil, true, errors.New(nodeSpec.RDMAError)
	}
	return nodeSpec.RDMADevices, true, nil
}

func resolveNodeSpec(nodeName string, store *mockspec.Store) (*mockspec.ResolvedNodeSpec, error) {
	snap, err := store.Refresh()
	if snap == nil {
		if err != nil {
			return nil, err
		}
		return nil, errors.New("mock devices store has no valid snapshot")
	}
	return snap.Document.ResolveNode(nodeName), nil
}
