package service

import (
	"fmt"
	"sort"

	"k8s.io/apimachinery/pkg/runtime"

	worker "gpustack.ai/gpustack/api/worker/v1"
	"gpustack.ai/gpustack/pkg/utils/funcx"
	"gpustack.ai/gpustack/pkg/utils/stringx"
	"gpustack.ai/gpustack/pkg/workergateway/manager"
)

func (in *AggregatedInstanceType) RecomputeOnceMaxRequest() {
	var newOverview AggregatedInstanceTypeOverviewResource

	for i := range in.Status.AcceleratorTiers {
		tier := &in.Status.AcceleratorTiers[i]
		if newOverview.Accelerator.Cmp(tier.OnceMaxRequest.Accelerator) < 0 {
			newOverview.Accelerator = tier.OnceMaxRequest.Accelerator
		}
		if newOverview.CPU.Cmp(tier.OnceMaxRequest.CPU) < 0 {
			newOverview.CPU = tier.OnceMaxRequest.CPU
		}
		if newOverview.RAM.Cmp(tier.OnceMaxRequest.RAM) < 0 {
			newOverview.RAM = tier.OnceMaxRequest.RAM
		}
		if newOverview.LocalStorage.Cmp(tier.OnceMaxRequest.LocalStorage) < 0 {
			newOverview.LocalStorage = tier.OnceMaxRequest.LocalStorage
		}
	}

	in.Status.OnceMaxRequest = newOverview
}

func (in *AggregatedInstanceTypeOnceMaxRequestTier) RecomputeOnceMaxRequest() {
	var newOverview AggregatedInstanceTypeOverviewResource

	for i := range in.Candidates {
		candidate := &in.Candidates[i]
		if newOverview.Accelerator.Cmp(candidate.Accelerator.OnceMaxRequest) < 0 {
			newOverview.Accelerator = candidate.Accelerator.OnceMaxRequest
		}
		if newOverview.CPU.Cmp(candidate.CPU.OnceMaxRequest) < 0 {
			newOverview.CPU = candidate.CPU.OnceMaxRequest
		}
		if newOverview.RAM.Cmp(candidate.RAM.OnceMaxRequest) < 0 {
			newOverview.RAM = candidate.RAM.OnceMaxRequest
		}
		if newOverview.LocalStorage.Cmp(candidate.LocalStorage.OnceMaxRequest) < 0 {
			newOverview.LocalStorage = candidate.LocalStorage.OnceMaxRequest
		}
	}

	in.OnceMaxRequest = newOverview
}

type ListAggregateInstanceTypes struct {
	list            AggregatedInstanceTypeList
	itemIndexer     map[AggregatedInstanceTypeSpec]int
	itemTierIndexer []map[string]int
}

func OpListAggregateInstanceTypes() *ListAggregateInstanceTypes {
	return &ListAggregateInstanceTypes{
		list: AggregatedInstanceTypeList{
			Items: make([]AggregatedInstanceType, 0),
		},
		itemIndexer: make(map[AggregatedInstanceTypeSpec]int),
	}
}

func (in *ListAggregateInstanceTypes) Next(cluster string, obj runtime.Object) error {
	instType, ok := obj.(*worker.InstanceType)
	if !ok {
		return fmt.Errorf("object is not of type InstanceType")
	}

	itemIndex, existed := in.itemIndexer[instType.Spec]
	if !existed {
		itemIndex = len(in.list.Items)
		in.itemIndexer[instType.Spec] = itemIndex
		in.itemTierIndexer = append(in.itemTierIndexer, make(map[string]int))
		item := AggregatedInstanceType{
			Name: funcx.Ternary(instType.GenerateName != "", stringx.TrimSuffix(instType.GenerateName, "-"), instType.Name),
			Spec: instType.Spec,
		}
		in.list.Items = append(in.list.Items, item)
	}

	item := &in.list.Items[itemIndex]
	tierIndexer := in.itemTierIndexer[itemIndex]

	tierIndexKey := instType.Status.Accelerator.OnceMaxRequest.String()
	tierIndex, existed := tierIndexer[tierIndexKey]
	if !existed {
		tierIndex = len(item.Status.AcceleratorTiers)
		tierIndexer[tierIndexKey] = tierIndex
		tier := AggregatedInstanceTypeOnceMaxRequestTier{
			OnceMaxRequest: AggregatedInstanceTypeOverviewResource{
				Accelerator:  instType.Status.Accelerator.OnceMaxRequest,
				CPU:          instType.Status.CPU.OnceMaxRequest,
				RAM:          instType.Status.RAM.OnceMaxRequest,
				LocalStorage: instType.Status.LocalStorage.OnceMaxRequest,
			},
		}
		item.Status.AcceleratorTiers = append(item.Status.AcceleratorTiers, tier)
	}

	tier := &item.Status.AcceleratorTiers[tierIndex]
	candidate := AggregatedInstanceTypeOnceMaxRequestCandidate{
		Cluster:      cluster,
		Name:         instType.Name,
		Accelerator:  instType.Status.Accelerator,
		CPU:          instType.Status.CPU,
		RAM:          instType.Status.RAM,
		LocalStorage: instType.Status.LocalStorage,
	}
	tier.Candidates = append(tier.Candidates, candidate)

	return nil
}

func (in *ListAggregateInstanceTypes) Result(sorted bool) AggregatedInstanceTypeList {
	if sorted {
		// Sorted by acceleratable and name for better readability.
		sort.Slice(in.list.Items, func(i, j int) bool {
			if in.list.Items[i].Spec.Acceleratable == in.list.Items[j].Spec.Acceleratable && in.list.Items[i].Spec.Acceleratable {
				return in.list.Items[i].Name < in.list.Items[j].Name
			}
			return in.list.Items[i].Spec.Acceleratable
		})
	}

	for i := range in.list.Items {
		item := &in.list.Items[i]

		// Sorted by once max request of accelerator for better readability.
		sort.Slice(item.Status.AcceleratorTiers, func(i, j int) bool {
			return item.Status.AcceleratorTiers[i].OnceMaxRequest.Accelerator.Cmp(item.Status.AcceleratorTiers[j].OnceMaxRequest.Accelerator) < 0
		})

		// Calculate the once max request of each tier.
		for j := range item.Status.AcceleratorTiers {
			tier := &item.Status.AcceleratorTiers[j]
			tier.RecomputeOnceMaxRequest()
		}

		// Calculate the once max request of the item.
		item.RecomputeOnceMaxRequest()
	}
	return in.list
}

type HandleAggregatedInstanceType struct {
	state AggregatedInstanceTypeList
}

func OpHandleAggregatedInstanceType(state AggregatedInstanceTypeList) *HandleAggregatedInstanceType {
	return &HandleAggregatedInstanceType{
		state: state,
	}
}

func (in *HandleAggregatedInstanceType) Handle(evt *manager.WorkerEvent) []*manager.WorkerEvent {
	var evts []*manager.WorkerEvent

	if evt.Type == manager.WorkerEventDeleted && evt.Object == nil {
		// Delete all instance types of the cluster.
		for i := 0; i < len(in.state.Items); i++ {
			item := &in.state.Items[i]
			itemChanged := false
			for j := 0; j < len(item.Status.AcceleratorTiers); j++ {
				tier := &item.Status.AcceleratorTiers[j]
				tierChanged := false
				// Keep the candidates of other clusters and delete the candidates of the cluster.
				newCandidates := make([]AggregatedInstanceTypeOnceMaxRequestCandidate, 0, len(tier.Candidates))
				for k := range tier.Candidates {
					candidate := tier.Candidates[k]
					if candidate.Cluster != evt.Cluster {
						newCandidates = append(newCandidates, tier.Candidates[k])
						continue
					}
					tierChanged = true
				}
				if !tierChanged {
					continue
				}
				itemChanged = true
				if len(newCandidates) == 0 {
					item.Status.AcceleratorTiers = append(item.Status.AcceleratorTiers[:j], item.Status.AcceleratorTiers[j+1:]...)
					j--
					continue
				}
				tier.Candidates = newCandidates

				// Recompute the tier.
				tier.RecomputeOnceMaxRequest()
			}
			if !itemChanged {
				continue
			}
			if len(item.Status.AcceleratorTiers) == 0 {
				itemName := item.Name
				in.state.Items = append(in.state.Items[:i], in.state.Items[i+1:]...)
				i--

				// Report a deleted event.
				evts = append(evts, &manager.WorkerEvent{
					Type:   manager.WorkerEventDeleted,
					Object: &AggregatedInstanceType{Name: itemName},
				})
				continue
			}

			// Recompute the item.
			item.RecomputeOnceMaxRequest()

			// Report a modified event.
			evts = append(evts, &manager.WorkerEvent{
				Type:   manager.WorkerEventModified,
				Object: item,
			})
		}

		return evts
	}

	instType, ok := evt.Object.(*worker.InstanceType)
	if !ok {
		return nil
	}
	if instType.DeletionTimestamp != nil {
		evt.Type = manager.WorkerEventDeleted
	}

	index := [3]int{-1, -1, -1} // item index, tier index, candidate index

	for i := 0; i < len(in.state.Items); i++ {
		if in.state.Items[i].Spec != instType.Spec {
			continue
		}
		index[0] = i

		item := &in.state.Items[index[0]]

		for j := 0; j < len(item.Status.AcceleratorTiers); j++ {
			tier := &item.Status.AcceleratorTiers[j]
			if tier.OnceMaxRequest.Accelerator.Equal(instType.Status.Accelerator.OnceMaxRequest) {
				index[1] = j
			}
			for k := range tier.Candidates {
				if tier.Candidates[k].Cluster == evt.Cluster && tier.Candidates[k].Name == instType.Name {
					index[1] = j
					index[2] = k
					break
				}
			}
			if index[2] != -1 {
				break
			}
		}

		// Found the same candidate.
		if index[2] != -1 {
			tier := &item.Status.AcceleratorTiers[index[1]]

			if evt.Type == manager.WorkerEventDeleted {
				// Remove candidate from the original tier.
				tier.Candidates = append(tier.Candidates[:index[2]], tier.Candidates[index[2]+1:]...)

				// Delete the original tier if no candidate.
				if len(tier.Candidates) == 0 {
					item.Status.AcceleratorTiers = append(item.Status.AcceleratorTiers[:index[1]], item.Status.AcceleratorTiers[index[1]+1:]...)
				} else {
					// Recompute the tier.
					tier.RecomputeOnceMaxRequest()
				}

				// Delete the original item if no tier.
				if len(item.Status.AcceleratorTiers) == 0 {
					itemName := item.Name
					in.state.Items = append(in.state.Items[:index[0]], in.state.Items[index[0]+1:]...)

					//  Report a deleted event also.
					evts = append(evts, &manager.WorkerEvent{
						Type:   manager.WorkerEventDeleted,
						Object: &AggregatedInstanceType{Name: itemName},
					})
				} else {
					// Recompute the item.
					item.RecomputeOnceMaxRequest()

					// Report a modified event.
					evts = append(evts, &manager.WorkerEvent{
						Type:   manager.WorkerEventModified,
						Object: item,
					})
				}

				// Return immediately since the deleted candidate will not be moved to another tier.
				return evts
			}

			candidate := &AggregatedInstanceTypeOnceMaxRequestCandidate{
				Cluster:      evt.Cluster,
				Name:         instType.Name,
				Accelerator:  instType.Status.Accelerator,
				CPU:          instType.Status.CPU,
				RAM:          instType.Status.RAM,
				LocalStorage: instType.Status.LocalStorage,
			}

			if tier.OnceMaxRequest.Accelerator.Equal(candidate.Accelerator.OnceMaxRequest) {
				// If the once max request has not changed, update the candidate in place.
				tier.Candidates[index[2]] = *candidate

				// Recompute the tier.
				tier.RecomputeOnceMaxRequest()
			} else {
				// Remove candidate from the original tier.
				tier.Candidates = append(tier.Candidates[:index[2]], tier.Candidates[index[2]+1:]...)

				// Delete the original tier if no candidate.
				if len(tier.Candidates) == 0 {
					item.Status.AcceleratorTiers = append(item.Status.AcceleratorTiers[:index[1]], item.Status.AcceleratorTiers[index[1]+1:]...)
				} else {
					// Recompute the tier.
					tier.RecomputeOnceMaxRequest()
				}

				// Find the new tier to move in.
				newTierIndex := -1
				for j := 0; j < len(item.Status.AcceleratorTiers); j++ {
					if item.Status.AcceleratorTiers[j].OnceMaxRequest.Accelerator.Equal(instType.Status.Accelerator.OnceMaxRequest) {
						newTierIndex = j
						break
					}
				}
				if newTierIndex != -1 {
					// Move to the new tier.
					tier = &item.Status.AcceleratorTiers[newTierIndex]
					tier.Candidates = append(tier.Candidates, *candidate)

					// Recompute the tier.
					tier.RecomputeOnceMaxRequest()
				} else {
					// Append a new tier if not found.
					item.Status.AcceleratorTiers = append(item.Status.AcceleratorTiers,
						AggregatedInstanceTypeOnceMaxRequestTier{
							OnceMaxRequest: AggregatedInstanceTypeOverviewResource{
								Accelerator:  instType.Status.Accelerator.OnceMaxRequest,
								CPU:          instType.Status.CPU.OnceMaxRequest,
								RAM:          instType.Status.RAM.OnceMaxRequest,
								LocalStorage: instType.Status.LocalStorage.OnceMaxRequest,
							},
							Candidates: []AggregatedInstanceTypeOnceMaxRequestCandidate{*candidate},
						})

					// Sorted.
					sort.Slice(item.Status.AcceleratorTiers, func(i, j int) bool {
						return item.Status.AcceleratorTiers[i].OnceMaxRequest.Accelerator.Cmp(item.Status.AcceleratorTiers[j].OnceMaxRequest.Accelerator) < 0
					})
				}
			}

			// Recompute the item.
			item.RecomputeOnceMaxRequest()

			// Report a modified event.
			evts = append(evts, &manager.WorkerEvent{
				Type:   manager.WorkerEventModified,
				Object: item,
			})

			return evts
		}

		// Found the same tier, but not in any candidate.
		if index[1] != -1 {
			if evt.Type == manager.WorkerEventDeleted {
				// The candidate to delete does not exist; nothing to do.
				return evts
			}

			tier := &item.Status.AcceleratorTiers[index[1]]

			candidate := &AggregatedInstanceTypeOnceMaxRequestCandidate{
				Cluster:      evt.Cluster,
				Name:         instType.Name,
				Accelerator:  instType.Status.Accelerator,
				CPU:          instType.Status.CPU,
				RAM:          instType.Status.RAM,
				LocalStorage: instType.Status.LocalStorage,
			}

			tier.Candidates = append(tier.Candidates, *candidate)

			// Recompute the tier.
			tier.RecomputeOnceMaxRequest()

			// Recompute the item.
			item.RecomputeOnceMaxRequest()

			// Report a modified event.
			evts = append(evts, &manager.WorkerEvent{
				Type:   manager.WorkerEventModified,
				Object: item,
			})

			return evts
		}

		// Found the same item, but not in any tier.
		break
	}

	if evt.Type != manager.WorkerEventDeleted {
		tier := AggregatedInstanceTypeOnceMaxRequestTier{
			OnceMaxRequest: AggregatedInstanceTypeOverviewResource{
				Accelerator:  instType.Status.Accelerator.OnceMaxRequest,
				CPU:          instType.Status.CPU.OnceMaxRequest,
				RAM:          instType.Status.RAM.OnceMaxRequest,
				LocalStorage: instType.Status.LocalStorage.OnceMaxRequest,
			},
			Candidates: []AggregatedInstanceTypeOnceMaxRequestCandidate{
				{
					Cluster:      evt.Cluster,
					Name:         instType.Name,
					Accelerator:  instType.Status.Accelerator,
					CPU:          instType.Status.CPU,
					RAM:          instType.Status.RAM,
					LocalStorage: instType.Status.LocalStorage,
				},
			},
		}

		// Found the same item but not in any tier.
		if index[0] != -1 {
			item := &in.state.Items[index[0]]
			item.Status.AcceleratorTiers = append(item.Status.AcceleratorTiers, tier)

			// Sorted.
			sort.Slice(item.Status.AcceleratorTiers, func(i, j int) bool {
				return item.Status.AcceleratorTiers[i].OnceMaxRequest.Accelerator.Cmp(item.Status.AcceleratorTiers[j].OnceMaxRequest.Accelerator) < 0
			})

			// Recompute the item.
			item.RecomputeOnceMaxRequest()

			// Report a modified event.
			evts = append(evts, &manager.WorkerEvent{
				Type:   manager.WorkerEventModified,
				Object: item,
			})

			return evts
		}

		// Not found the same item, tier and candidate, create a new item with a new tier and candidate.
		in.state.Items = append(in.state.Items, AggregatedInstanceType{
			Name: funcx.Ternary(instType.GenerateName != "", stringx.TrimSuffix(instType.GenerateName, "-"), instType.Name),
			Spec: instType.Spec,
			Status: AggregatedInstanceTypeStatus{
				OnceMaxRequest: tier.OnceMaxRequest,
				AcceleratorTiers: []AggregatedInstanceTypeOnceMaxRequestTier{
					tier,
				},
			},
		})

		item := &in.state.Items[len(in.state.Items)-1]

		// Report an added event.
		evts = append(evts, &manager.WorkerEvent{
			Type:   manager.WorkerEventAdded,
			Object: item,
		})

		return evts
	}

	return evts
}

type ListClusterInstanceTypes struct {
	list ClusterInstanceTypeList
}

func OpListClusterInstanceTypes() *ListClusterInstanceTypes {
	return &ListClusterInstanceTypes{
		list: ClusterInstanceTypeList{
			Items: make([]ClusterInstanceType, 0),
		},
	}
}

func (in *ListClusterInstanceTypes) Next(cluster string, obj runtime.Object) error {
	instType, ok := obj.(*worker.InstanceType)
	if !ok {
		return fmt.Errorf("object is not of type InstanceType")
	}

	item := ClusterInstanceType{
		InstanceType: *instType,
		Cluster:      cluster,
	}
	item.ManagedFields = nil
	in.list.Items = append(in.list.Items, item)
	return nil
}

func (in *ListClusterInstanceTypes) Result() ClusterInstanceTypeList {
	return in.list
}

type HandleClusterInstanceType struct{}

func OpHandleClusterInstanceType() *HandleClusterInstanceType {
	return &HandleClusterInstanceType{}
}

func (in *HandleClusterInstanceType) Handle(evt *manager.WorkerEvent) []*manager.WorkerEvent {
	if evt.Object != nil {
		instType, ok := evt.Object.(*worker.InstanceType)
		if !ok {
			return nil
		}
		if instType.DeletionTimestamp != nil {
			evt.Type = manager.WorkerEventDeleted
		}
		clusterInstType := &ClusterInstanceType{
			InstanceType: *instType,
			Cluster:      evt.Cluster,
		}
		clusterInstType.ManagedFields = nil
		evt.Object = clusterInstType
	}
	return []*manager.WorkerEvent{evt}
}

type ListClusterInstances struct {
	list ClusterInstanceList
}

func OpListClusterInstances() *ListClusterInstances {
	return &ListClusterInstances{
		list: ClusterInstanceList{
			Items: make([]ClusterInstance, 0),
		},
	}
}

func (in *ListClusterInstances) Next(cluster string, obj runtime.Object) error {
	inst, ok := obj.(*worker.Instance)
	if !ok {
		return fmt.Errorf("object is not of type Instance")
	}

	item := ClusterInstance{
		Instance: *inst,
		Cluster:  cluster,
	}
	item.ManagedFields = nil
	in.list.Items = append(in.list.Items, item)
	return nil
}

func (in *ListClusterInstances) Result() ClusterInstanceList {
	return in.list
}

type HandleClusterInstance struct {
	namespace string
}

func OpHandleClusterInstance(namespace string) *HandleClusterInstance {
	return &HandleClusterInstance{namespace: namespace}
}

func (in *HandleClusterInstance) Handle(evt *manager.WorkerEvent) []*manager.WorkerEvent {
	if evt.Object != nil {
		inst, ok := evt.Object.(*worker.Instance)
		if !ok {
			return nil
		}
		if in.namespace != "" && inst.Namespace != in.namespace {
			return nil
		}
		if inst.DeletionTimestamp != nil {
			evt.Type = manager.WorkerEventDeleted
		}
		clusterInst := &ClusterInstance{
			Instance: *inst,
			Cluster:  evt.Cluster,
		}
		clusterInst.ManagedFields = nil
		evt.Object = clusterInst
	}
	return []*manager.WorkerEvent{evt}
}

type ListClusterInstancePersistentVolumeTypes struct {
	list ClusterInstancePersistentVolumeTypeList
}

func OpListClusterInstancePersistentVolumeTypes() *ListClusterInstancePersistentVolumeTypes {
	return &ListClusterInstancePersistentVolumeTypes{
		list: ClusterInstancePersistentVolumeTypeList{
			Items: make([]ClusterInstancePersistentVolumeType, 0),
		},
	}
}

func (in *ListClusterInstancePersistentVolumeTypes) Next(cluster string, obj runtime.Object) error {
	volType, ok := obj.(*worker.InstancePersistentVolumeType)
	if !ok {
		return fmt.Errorf("object is not of type InstancePersistentVolumeType")
	}

	item := ClusterInstancePersistentVolumeType{
		InstancePersistentVolumeType: *volType,
		Cluster:                      cluster,
	}
	item.ManagedFields = nil
	in.list.Items = append(in.list.Items, item)
	return nil
}

func (in *ListClusterInstancePersistentVolumeTypes) Result() ClusterInstancePersistentVolumeTypeList {
	return in.list
}

type HandleClusterInstancePersistentVolumeType struct{}

func OpHandleClusterInstancePersistentVolumeType() *HandleClusterInstancePersistentVolumeType {
	return &HandleClusterInstancePersistentVolumeType{}
}

func (in *HandleClusterInstancePersistentVolumeType) Handle(evt *manager.WorkerEvent) []*manager.WorkerEvent {
	if evt.Object != nil {
		volType, ok := evt.Object.(*worker.InstancePersistentVolumeType)
		if !ok {
			return nil
		}
		if volType.DeletionTimestamp != nil {
			evt.Type = manager.WorkerEventDeleted
		}
		clusterVolType := &ClusterInstancePersistentVolumeType{
			InstancePersistentVolumeType: *volType,
			Cluster:                      evt.Cluster,
		}
		clusterVolType.ManagedFields = nil
		evt.Object = clusterVolType
	}
	return []*manager.WorkerEvent{evt}
}

type ListClusterInstancePersistentVolumes struct {
	list ClusterInstancePersistentVolumeList
}

func OpListClusterInstancePersistentVolumes() *ListClusterInstancePersistentVolumes {
	return &ListClusterInstancePersistentVolumes{}
}

func (in *ListClusterInstancePersistentVolumes) Next(cluster string, obj runtime.Object) error {
	vol, ok := obj.(*worker.InstancePersistentVolume)
	if !ok {
		return fmt.Errorf("object is not of type InstancePersistentVolume")
	}

	item := ClusterInstancePersistentVolume{
		InstancePersistentVolume: *vol,
		Cluster:                  cluster,
	}
	item.ManagedFields = nil
	in.list.Items = append(in.list.Items, item)
	return nil
}

func (in *ListClusterInstancePersistentVolumes) Result() ClusterInstancePersistentVolumeList {
	return in.list
}

type HandleClusterInstancePersistentVolume struct {
	namespace string
}

func OpHandleClusterInstancePersistentVolume(namespace string) *HandleClusterInstancePersistentVolume {
	return &HandleClusterInstancePersistentVolume{namespace: namespace}
}

func (in *HandleClusterInstancePersistentVolume) Handle(evt *manager.WorkerEvent) []*manager.WorkerEvent {
	if evt.Object != nil {
		vol, ok := evt.Object.(*worker.InstancePersistentVolume)
		if !ok {
			return nil
		}
		if in.namespace != "" && vol.Namespace != in.namespace {
			return nil
		}
		if vol.DeletionTimestamp != nil {
			evt.Type = manager.WorkerEventDeleted
		}
		clusterVol := &ClusterInstancePersistentVolume{
			InstancePersistentVolume: *vol,
			Cluster:                  evt.Cluster,
		}
		clusterVol.ManagedFields = nil
		evt.Object = clusterVol
	}
	return []*manager.WorkerEvent{evt}
}

type ListClusterInstanceImagePullSecrets struct {
	list ClusterInstanceImagePullSecretList
}

func OpListClusterInstanceImagePullSecrets() *ListClusterInstanceImagePullSecrets {
	return &ListClusterInstanceImagePullSecrets{
		list: ClusterInstanceImagePullSecretList{
			Items: make([]ClusterInstanceImagePullSecret, 0),
		},
	}
}

func (in *ListClusterInstanceImagePullSecrets) Next(cluster string, obj runtime.Object) error {
	secret, ok := obj.(*worker.InstanceImagePullSecret)
	if !ok {
		return fmt.Errorf("object is not of type InstanceImagePullSecret")
	}

	item := ClusterInstanceImagePullSecret{
		InstanceImagePullSecret: *secret,
		Cluster:                 cluster,
	}
	item.ManagedFields = nil
	in.list.Items = append(in.list.Items, item)
	return nil
}

func (in *ListClusterInstanceImagePullSecrets) Result() ClusterInstanceImagePullSecretList {
	return in.list
}

type HandleClusterInstanceImagePullSecret struct {
	namespace string
}

func OpHandleClusterInstanceImagePullSecret(namespace string) *HandleClusterInstanceImagePullSecret {
	return &HandleClusterInstanceImagePullSecret{namespace: namespace}
}

func (in *HandleClusterInstanceImagePullSecret) Handle(evt *manager.WorkerEvent) []*manager.WorkerEvent {
	if evt.Object != nil {
		secret, ok := evt.Object.(*worker.InstanceImagePullSecret)
		if !ok {
			return nil
		}
		if in.namespace != "" && secret.Namespace != in.namespace {
			return nil
		}
		if secret.DeletionTimestamp != nil {
			evt.Type = manager.WorkerEventDeleted
		}
		clusterSecret := &ClusterInstanceImagePullSecret{
			InstanceImagePullSecret: *secret,
			Cluster:                 evt.Cluster,
		}
		clusterSecret.ManagedFields = nil
		evt.Object = clusterSecret
	}
	return []*manager.WorkerEvent{evt}
}

type ListClusterInstanceSSHPublicKeys struct {
	list ClusterInstanceSSHPublicKeyList
}

func OpListClusterInstanceSSHPublicKeys() *ListClusterInstanceSSHPublicKeys {
	return &ListClusterInstanceSSHPublicKeys{
		list: ClusterInstanceSSHPublicKeyList{
			Items: make([]ClusterInstanceSSHPublicKey, 0),
		},
	}
}

func (in *ListClusterInstanceSSHPublicKeys) Next(cluster string, obj runtime.Object) error {
	key, ok := obj.(*worker.InstanceSSHPublicKey)
	if !ok {
		return fmt.Errorf("object is not of type InstanceSSHPublicKey")
	}

	item := ClusterInstanceSSHPublicKey{
		InstanceSSHPublicKey: *key,
		Cluster:              cluster,
	}
	item.ManagedFields = nil
	in.list.Items = append(in.list.Items, item)
	return nil
}

func (in *ListClusterInstanceSSHPublicKeys) Result() ClusterInstanceSSHPublicKeyList {
	return in.list
}

type HandleClusterInstanceSSHPublicKey struct {
	namespace string
}

func OpHandleClusterInstanceSSHPublicKey(namespace string) *HandleClusterInstanceSSHPublicKey {
	return &HandleClusterInstanceSSHPublicKey{namespace: namespace}
}

func (in *HandleClusterInstanceSSHPublicKey) Handle(evt *manager.WorkerEvent) []*manager.WorkerEvent {
	if evt.Object != nil {
		key, ok := evt.Object.(*worker.InstanceSSHPublicKey)
		if !ok {
			return nil
		}
		if in.namespace != "" && key.Namespace != in.namespace {
			return nil
		}
		if key.DeletionTimestamp != nil {
			evt.Type = manager.WorkerEventDeleted
		}
		clusterKey := &ClusterInstanceSSHPublicKey{
			InstanceSSHPublicKey: *key,
			Cluster:              evt.Cluster,
		}
		clusterKey.ManagedFields = nil
		evt.Object = clusterKey
	}
	return []*manager.WorkerEvent{evt}
}
