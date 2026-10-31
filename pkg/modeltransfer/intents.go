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
	var nodes core.NodeList
	if err := s.client.List(ctx, &nodes); err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	var node *core.Node
	for i := range nodes.Items {
		if nodes.Items[i].Name == c.NodeName {
			node = &nodes.Items[i]
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
	for i := range prefetches.Items {
		pf := &prefetches.Items[i]
		if pf.Spec.Source == nil || pf.DeletionTimestamp != nil || pf.UID == "" || pf.Generation < 1 {
			continue
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
		targets, err := modelstore.PrefetchSourceTargetNodes(pf, nodes.Items, pools)
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
		claimed, err := c.Store.Claim(store.Ref{
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
