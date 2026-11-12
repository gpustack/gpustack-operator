package modeltransfer

import (
	"context"
	"errors"
	"strconv"
	"time"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/modelstore"
	"gpustack.ai/gpustack/pkg/modeltransfer/broker"
	"gpustack.ai/gpustack/pkg/worker/settings"
)

// validateLiveIntent reuses positive entitlement checks for at most ten seconds.
func (c *Config) validateLiveIntent(ctx context.Context, intent broker.LiveIntent) error {
	c.liveIntentMu.Lock()
	now := time.Now()
	if expires, ok := c.liveIntentCache[intent]; ok && now.Before(expires) {
		c.liveIntentMu.Unlock()
		return nil
	}
	for key, expires := range c.liveIntentCache {
		if !now.Before(expires) {
			delete(c.liveIntentCache, key)
		}
	}
	epoch := c.liveIntentEpoch
	c.liveIntentMu.Unlock()
	if err := c.validateLiveIntentUncached(ctx, intent); err != nil {
		return err
	}
	c.liveIntentMu.Lock()
	defer c.liveIntentMu.Unlock()
	if epoch != c.liveIntentEpoch {
		return nil
	}
	if c.liveIntentCache == nil {
		c.liveIntentCache = make(map[broker.LiveIntent]time.Time)
	}
	c.liveIntentCache[intent] = now.Add(10 * time.Second)
	return nil
}

func (c *Config) invalidateLiveIntentCache() {
	c.liveIntentMu.Lock()
	defer c.liveIntentMu.Unlock()
	c.liveIntentEpoch++
	c.liveIntentCache = nil
}

func (c *Config) validateLiveIntentUncached(ctx context.Context, intent broker.LiveIntent) error {
	if intent.NodeName != c.NodeName || intent.WorkerID != c.WorkerID {
		return errors.New("source identity changed")
	}
	if err := c.sourcePod(ctx); err != nil {
		return err
	}
	src, ok := c.Intents.(*kubernetesIntentSource)
	if !ok {
		return errors.New("source reader unavailable")
	}
	reader := src.client
	pf := &workercore.ModelPrefetch{}
	if err := reader.Get(ctx, ctrlcli.ObjectKey{Namespace: intent.Namespace, Name: intent.IntentName}, pf); err != nil {
		return err
	}
	if string(pf.UID) != intent.IntentUID || pf.Generation != intent.IntentGeneration || pf.DeletionTimestamp != nil ||
		pf.Spec.Source == nil || pf.Spec.BindingRef.Name != intent.BindingName || pf.Spec.ArtifactRef.Name != intent.ArtifactName {
		return errors.New("source intent changed")
	}
	binding := &workercore.ModelStoreBinding{}
	if err := reader.Get(ctx, ctrlcli.ObjectKey{Namespace: intent.Namespace, Name: intent.BindingName}, binding); err != nil {
		return err
	}
	// A Binding that names no sourceMemory section keeps following the
	// node's configured capacity; only identity, deletion, or a pin change
	// invalidates the live grant.
	if string(binding.UID) != intent.BindingUID || binding.DeletionTimestamp != nil {
		return errors.New("source grant changed")
	}
	model := &workercore.ModelArtifact{}
	if err := reader.Get(ctx, ctrlcli.ObjectKey{Namespace: intent.Namespace, Name: intent.ArtifactName}, model); err != nil {
		return err
	}
	if string(model.UID) != intent.ArtifactUID || model.Generation != intent.SourceGeneration ||
		model.Status.ObservedGeneration != model.Generation || model.DeletionTimestamp != nil || model.Status.Resolved == nil ||
		model.Status.Resolved.ManifestDigest != intent.Digest || model.Status.Resolved.SizeBytes <= 0 {
		return errors.New("source artifact changed")
	}
	ready := false
	for _, condition := range model.Status.Conditions {
		if condition.Type == "Resolved" && condition.Status == meta.ConditionTrue {
			ready = true
			break
		}
	}
	if !ready {
		return errors.New("source artifact is not resolved")
	}
	var nodes core.NodeList
	if err := reader.List(ctx, &nodes); err != nil {
		return err
	}
	var node *core.Node
	for i := range nodes.Items {
		if nodes.Items[i].Name == c.NodeName {
			node = &nodes.Items[i]
			break
		}
	}
	if node == nil {
		return errors.New("source node is unavailable")
	}
	covered, err := modelstore.BindingCoversNode(ctx, reader, binding, node)
	if err != nil {
		return err
	}
	if !covered {
		return errors.New("source node is outside the Binding")
	}
	var pools modelstore.AcceleratorTypeFlavorNames
	if pf.Spec.Placement != nil && len(pf.Spec.Placement.AcceleratorTypes) > 0 {
		setting, err := c.Client.GpustackV1().Settings(c.WorkerNamespace).Get(ctx, settings.InstanceTypeAwareCPUManufacturer.Name(), meta.GetOptions{})
		if err != nil {
			return err
		}
		cpuAware, err := strconv.ParseBool(setting.Status.Value)
		if err != nil {
			return err
		}
		pools, err = modelstore.ResolvePlacementAcceleratorTypeFlavorNames(ctx, reader, pf.Spec.Placement.AcceleratorTypes, cpuAware)
		if err != nil {
			return err
		}
	}
	targets, err := modelstore.PrefetchSourceTargetNodes(pf, nodes.Items, pools)
	if err != nil {
		return err
	}
	for _, name := range targets {
		if name == c.NodeName {
			return nil
		}
	}
	return errors.New("source node is outside the intent")
}

// isSessionPodLive reports whether the target Pod that opened a session still
// exists and has not terminated. An API error returns an error so the caller
// never treats a failed read as Pod disappearance. A namespace's answer is
// reused for a short window so many sessions of one owner share one read
// instead of listing the namespace again for each of them.
func (c *Config) isSessionPodLive(ctx context.Context, namespace, podUID string) (bool, error) {
	if namespace == "" || podUID == "" {
		return false, errors.New("session Pod identity is incomplete")
	}
	src, ok := c.Intents.(*kubernetesIntentSource)
	if !ok || src.client == nil {
		return false, errors.New("session Pod reader is unavailable")
	}
	now := time.Now()
	c.sessionPodMu.Lock()
	snapshot, cached := c.sessionPodCache[namespace]
	c.sessionPodMu.Unlock()
	if !cached || !now.Before(snapshot.expires) {
		live, err := listSessionPodsLive(ctx, src.client, namespace)
		if err != nil {
			return false, err
		}
		c.sessionPodMu.Lock()
		if c.sessionPodCache == nil {
			c.sessionPodCache = make(map[string]sessionPodSnapshot)
		}
		for key, stale := range c.sessionPodCache {
			if !now.Before(stale.expires) {
				delete(c.sessionPodCache, key)
			}
		}
		c.sessionPodCache[namespace] = sessionPodSnapshot{
			live: live, expires: now.Add(sessionPodCacheTTL),
		}
		c.sessionPodMu.Unlock()
		snapshot = sessionPodSnapshot{live: live}
	}
	return snapshot.live[podUID], nil
}

// listSessionPodsLive maps every Pod in the namespace to whether it still
// runs: terminated phases and fully terminated containers read as gone.
func listSessionPodsLive(ctx context.Context, reader ctrlcli.Reader, namespace string) (map[string]bool, error) {
	var pods core.PodList
	if err := reader.List(ctx, &pods, ctrlcli.InNamespace(namespace)); err != nil {
		return nil, err
	}
	live := make(map[string]bool, len(pods.Items))
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.UID == "" {
			continue
		}
		if pod.Status.Phase == core.PodSucceeded || pod.Status.Phase == core.PodFailed {
			live[string(pod.UID)] = false
			continue
		}
		if len(pod.Status.ContainerStatuses) > 0 {
			allTerminated := true
			for _, status := range pod.Status.ContainerStatuses {
				if status.State.Terminated == nil {
					allTerminated = false
					break
				}
			}
			if allTerminated {
				live[string(pod.UID)] = false
				continue
			}
		}
		live[string(pod.UID)] = true
	}
	return live, nil
}
