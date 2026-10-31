package modeltransfer

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"

	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/modelmanager/store"
	"gpustack.ai/gpustack/pkg/modelstore"
	"gpustack.ai/gpustack/pkg/modeltransfer/artifact"
	"gpustack.ai/gpustack/pkg/modeltransfer/broker"
	"gpustack.ai/gpustack/pkg/modeltransfer/protocol"
	"gpustack.ai/gpustack/pkg/worker/settings"
)

type kubernetesIntentSource struct {
	config   *Config
	client   ctrlcli.Client
	eligible bool
}

// candidateSnapshot returns the synced candidate view a pass selects from,
// or nothing when the caches are not ready, in which case the pass lists
// the fleet live exactly as before.
func (c *Config) candidateSnapshot() ([]core.Node, []workercore.ModelPrefetch, bool) {
	if snap := c.snapshot; snap != nil && snap.Ready() {
		return snap.Nodes(), snap.Prefetches(), true
	}
	return nil, nil, false
}

// placementCouldTarget reports, from cached placement facts alone, whether a
// snapshot prefetch could still name this node, so a candidate the cached
// placement provably excludes is never read live. A placement without a
// source target set, an ambiguous one, and an unparsable nodeSelector
// nominate nothing. An InstanceType-only placement cannot be excluded
// without the pool resolution, so it stays a candidate and its placement is
// resolved live. A stale negative waits for its watch event rather than
// forcing a read; a nomination is never itself a positive — the live pass
// still re-resolves the placement and re-targets the node before anything
// is admitted.
func placementCouldTarget(pf *workercore.ModelPrefetch, node *core.Node) bool {
	placement := pf.Spec.Placement
	switch {
	case placement == nil || (len(placement.InstanceTypes) == 0 && placement.NodeSelector == nil):
		return false
	case len(placement.InstanceTypes) > 0 && placement.NodeSelector != nil:
		return false
	case placement.NodeSelector != nil:
		selector, err := meta.LabelSelectorAsSelector(placement.NodeSelector)
		if err != nil {
			return false
		}
		return selector.Matches(labels.Set(node.Labels))
	default:
		return true
	}
}

// ListIntents resolves explicit source placements and live grants. Any API
// failure aborts the pass so a read failure cannot retire existing work.
func (s *kubernetesIntentSource) ListIntents(ctx context.Context) (_ []broker.Intent, retErr error) {
	c := s.config
	s.eligible = false
	if err := c.Broker.VerifyAuthorityInventory(ctx, s.client, c.Client.CoreV1().ConfigMaps(c.WorkerNamespace), c.WorkerNamespace); err != nil {
		return nil, err
	}
	if err := c.Broker.RecoverReservationAuthorities(ctx, s.client, c.Client.CoreV1().ConfigMaps(c.WorkerNamespace), c.WorkerNamespace); err != nil {
		return nil, err
	}
	if err := c.sourcePod(ctx); err != nil {
		if errors.Is(err, errSourceIneligible) {
			return nil, nil
		}
		return nil, fmt.Errorf("read source Pod: %w", err)
	}
	nodeItems, prefetchItems, snapshotted := c.candidateSnapshot()
	var node *core.Node
	if snapshotted {
		// The live node answers every admission question about this node —
		// presence, placement labels, and the grant's coverage target. The
		// cached fleet only narrows which prefetches are worth reading.
		node = &core.Node{}
		if err := s.client.Get(ctx, ctrlcli.ObjectKey{Name: c.NodeName}, node); err != nil {
			return nil, fmt.Errorf("read source node: %w", err)
		}
		for i := range nodeItems {
			if nodeItems[i].Name == c.NodeName {
				nodeItems[i] = *node
				break
			}
		}
	} else {
		var nodes core.NodeList
		if err := s.client.List(ctx, &nodes); err != nil {
			return nil, fmt.Errorf("list nodes: %w", err)
		}
		nodeItems = nodes.Items
		for i := range nodeItems {
			if nodeItems[i].Name == c.NodeName {
				node = &nodeItems[i]
				break
			}
		}
		if node == nil {
			return nil, fmt.Errorf("source node disappeared during listing")
		}
		var prefetches workercore.ModelPrefetchList
		if err := s.client.List(ctx, &prefetches); err != nil {
			return nil, fmt.Errorf("list source prefetches: %w", err)
		}
		prefetchItems = prefetches.Items
	}
	var intents []broker.Intent
	defer func() {
		if retErr != nil {
			for i := range intents {
				if intents[i].ReleaseClaim != nil {
					intents[i].ReleaseClaim()
				}
			}
		}
	}()
	var cpuAware *bool
	for i := range prefetchItems {
		cached := &prefetchItems[i]
		if cached.Spec.Source == nil || cached.DeletionTimestamp != nil || cached.UID == "" || cached.Generation < 1 {
			continue
		}
		pf := cached
		if snapshotted {
			// The cached fleet narrows which prefetches are worth reading:
			// one whose cached placement provably excludes this node's live
			// labels is skipped without a read. The exclusion is a
			// stale-safe negative — a placement edit that would re-include
			// this node arrives with its watch event and is then read and
			// admitted.
			if !placementCouldTarget(cached, node) {
				continue
			}
			// The snapshot only nominates candidates. A prefetch whose
			// live identity moved on — deleted, withdrawn, or a new
			// generation — is skipped; the refreshed snapshot names it
			// again once its watch event lands.
			live := &workercore.ModelPrefetch{}
			if err := s.client.Get(ctx, ctrlcli.ObjectKeyFromObject(cached), live); err != nil {
				if apierrors.IsNotFound(err) {
					continue
				}
				return nil, fmt.Errorf("read source prefetch: %w", err)
			}
			if string(live.UID) != string(cached.UID) || live.Generation != cached.Generation ||
				live.DeletionTimestamp != nil || live.Spec.Source == nil {
				continue
			}
			pf = live
		}
		var pools modelstore.InstanceTypeFlavorNames
		if pf.Spec.Placement != nil && len(pf.Spec.Placement.InstanceTypes) > 0 {
			if cpuAware == nil {
				setting, err := c.Client.GpustackV1().Settings(c.WorkerNamespace).Get(ctx, settings.InstanceTypeAwareCPUManufacturer.Name(), meta.GetOptions{})
				if err != nil {
					return nil, fmt.Errorf("read CPU-aware setting: %w", err)
				}
				value, err := strconv.ParseBool(setting.Status.Value)
				if err != nil {
					return nil, fmt.Errorf("invalid CPU-aware setting: %w", err)
				}
				cpuAware = &value
			}
			var err error
			pools, err = modelstore.ResolvePlacementInstanceTypeFlavorNames(ctx, s.client, pf.Spec.Placement.InstanceTypes, *cpuAware)
			if err != nil {
				return nil, fmt.Errorf("resolve placement: %w", err)
			}
		}
		targets, err := modelstore.PrefetchSourceTargetNodes(pf, nodeItems, pools)
		if err != nil {
			ctrllog.FromContext(ctx).Error(err, "skip source prefetch with invalid placement", "prefetch", ctrlcli.ObjectKeyFromObject(pf))
			continue
		}
		target := false
		for _, name := range targets {
			if name == c.NodeName {
				target = true
				break
			}
		}
		if !target {
			continue
		}
		binding := &workercore.ModelStoreBinding{}
		if err := s.client.Get(ctx, ctrlcli.ObjectKey{Namespace: pf.Namespace, Name: pf.Spec.BindingRef.Name}, binding); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("read Binding: %w", err)
		}
		// A Binding that names no sourceMemory section follows the node's
		// own configured capacity, so model-transfer tracks NodeModelStore
		// and ModelPrefetch without a mandatory Binding edit.
		if binding.UID == "" || binding.DeletionTimestamp != nil {
			continue
		}
		grant := broker.ResolveGrant(c.Broker.LimitsSnapshot(), binding.Spec.SourceMemory, binding.UID)
		covered, err := modelstore.BindingCoversNode(ctx, s.client, binding, node)
		if err != nil {
			return nil, fmt.Errorf("read Binding coverage: %w", err)
		}
		if !covered {
			continue
		}
		model := &workercore.ModelArtifact{}
		if err := s.client.Get(ctx, ctrlcli.ObjectKey{Namespace: pf.Namespace, Name: pf.Spec.ArtifactRef.Name}, model); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("read ModelArtifact: %w", err)
		}
		resolved := model.Status.Resolved
		if model.UID == "" || model.Generation < 1 || model.Status.ObservedGeneration != model.Generation ||
			model.DeletionTimestamp != nil || resolved == nil || resolved.SizeBytes <= 0 ||
			store.HexOf(resolved.ManifestDigest) == "" ||
			(model.Spec.Source.HuggingFace == nil && model.Spec.Source.ModelScope == nil) {
			continue
		}
		ready := false
		for _, condition := range model.Status.Conditions {
			if condition.Type == "Resolved" && condition.Status == meta.ConditionTrue {
				ready = true
				break
			}
		}
		if !ready {
			continue
		}
		identity, err := json.Marshal([]any{
			string(pf.UID), pf.Generation, string(binding.UID), string(model.UID), model.Generation,
			resolved.ManifestDigest, c.WorkerID,
		})
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(identity)
		key := hex.EncodeToString(sum[:])
		backend := "nixl"
		if c.VendorScope == "ascend" {
			backend = "hixl"
		}
		intent := broker.Intent{
			Key: key, Namespace: pf.Namespace, Digest: resolved.ManifestDigest,
			Grant: broker.Grant{
				Namespace: pf.Namespace, Bytes: grant.Bytes,
				Registrations: grant.Registrations, Sessions: grant.Sessions,
			},
			WorkerID: c.WorkerID, NodeName: c.NodeName, IntentUID: string(pf.UID), IntentGeneration: pf.Generation,
			IntentName: pf.Name, ArtifactName: model.Name,
			BindingUID: string(binding.UID), BindingNamespace: binding.Namespace, BindingName: binding.Name,
			ArtifactUID: string(model.UID), SourceGeneration: model.Generation, Backend: backend,
		}
		if existing, ok := c.Broker.AssignmentForIntent(key); ok {
			intent.TotalBytes, intent.Registrations = existing.TotalBytes, existing.Registrations
			intent.ManifestSHA256, intent.ManifestJSON = existing.ManifestSHA256, existing.ManifestJSON
			intent.TreePath = existing.TreePath
			intents = append(intents, intent)
			continue
		}
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return nil, err
		}
		token := hex.EncodeToString(nonce[:])
		ref := buildRefPrefix(c.VendorScope) + token
		claimed, err := c.Store.Claim(ctx, store.Ref{
			VolumeID: ref, Hex: store.HexOf(resolved.ManifestDigest),
			PodNamespace: c.WorkerNamespace, PodUID: c.WorkerID,
		})
		if err != nil {
			return nil, fmt.Errorf("claim cache: %w", err)
		}
		if !claimed {
			continue
		}
		pageRoot := c.manifestPageRoot
		if err := os.MkdirAll(pageRoot, 0o700); err != nil {
			_ = c.Store.RemoveRef(ref)
			return nil, err
		}
		pageDirectory, err := os.MkdirTemp(pageRoot, "manifest-")
		if err != nil {
			_ = c.Store.RemoveRef(ref)
			return nil, err
		}
		view, err := artifact.BuildPaged(ctx, c.Store, string(model.UID), model.Generation, resolved.ManifestDigest, pageDirectory)
		if err != nil {
			ctrllog.FromContext(ctx).Error(err, "build source manifest", "prefetch", ctrlcli.ObjectKeyFromObject(pf), "artifact", model.Name)
			_ = c.Store.RemoveRef(ref)
			_ = os.RemoveAll(pageDirectory)
			continue
		}
		if len(view.Raw) > protocol.MaxManifestBytes {
			ctrllog.FromContext(ctx).Info("skip oversized source manifest",
				"prefetch", ctrlcli.ObjectKeyFromObject(pf), "artifact", model.Name,
				"bytes", len(view.Raw), "limit", protocol.MaxManifestBytes)
			_ = c.Store.RemoveRef(ref)
			_ = os.RemoveAll(pageDirectory)
			continue
		}
		encodedManifest, err := json.Marshal(string(view.Raw))
		if err != nil || len(encodedManifest) > protocol.MaxManifestResponseBytes {
			ctrllog.FromContext(ctx).Info("skip invalid source manifest response",
				"prefetch", ctrlcli.ObjectKeyFromObject(pf), "artifact", model.Name, "error", err,
				"bytes", len(encodedManifest), "limit", protocol.MaxManifestResponseBytes)
			_ = c.Store.RemoveRef(ref)
			_ = os.RemoveAll(pageDirectory)
			continue
		}
		authority, liveGrant, err := broker.EnsureReservationAuthority(
			ctx, s.client, c.Client.CoreV1().ConfigMaps(c.WorkerNamespace), c.WorkerNamespace,
			binding.Namespace, binding.Name, binding.UID, c.Broker.LimitsSnapshot(),
		)
		if err != nil {
			_ = c.Store.RemoveRef(ref)
			_ = os.RemoveAll(pageDirectory)
			return nil, fmt.Errorf("initialize Binding authority: %w", err)
		}
		c.Broker.SetReservationAuthority(binding.UID, authority, liveGrant)
		intent.TotalBytes, intent.Registrations = view.RegisteredBytes, int32(view.Registrations)
		intent.ReservationID, intent.ManifestSHA256, intent.ManifestJSON = token, view.SHA256, string(view.Raw)
		intent.TreePath = view.TreeDir
		intent.ManifestPagesPath = pageDirectory
		intent.ReleaseClaim = func() { _ = c.Store.RemoveRef(ref); _ = os.RemoveAll(pageDirectory) }
		intents = append(intents, intent)
	}
	s.eligible = true
	return intents, nil
}
