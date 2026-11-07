package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	klog "k8s.io/klog/v2"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

const authorityInventoryFile = "authority-inventory.json"

// authorityConfigMapDeleter is the optional deletion surface of the
// authority ConfigMap API. Deletion stays out of the required interface so
// existing readers keep implementing it unchanged; garbage-collecting an
// empty orphan is optional for read-only implementations.
type authorityConfigMapDeleter interface {
	Delete(context.Context, string, meta.DeleteOptions) error
}

type inventoryBinding struct {
	Namespace string       `json:"namespace"`
	Name      string       `json:"name"`
	Pin       AuthorityPin `json:"pin"`
}

type authorityInventory struct {
	Version        int                         `json:"version"`
	Node           string                      `json:"node"`
	Scope          string                      `json:"scope"`
	LedgerIdentity string                      `json:"ledgerIdentity"`
	Bindings       map[string]inventoryBinding `json:"bindings"`
}

// RequireAuthorityInventory enables crash-safe authority inventory for the
// production broker. An established ledger with a missing inventory holds.
func (b *Broker) RequireAuthorityInventory(node string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if node == "" {
		return fmt.Errorf("%w: inventory node is absent", ErrReservationUnavailable)
	}
	path := filepath.Join(b.root, authorityInventoryFile)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if !b.fresh {
			return fmt.Errorf("%w: established ledger has no authority inventory", ErrReservationUnavailable)
		}
		inventory := &authorityInventory{Version: 1, Node: node, Scope: b.scope, LedgerIdentity: b.ledger.identity, Bindings: map[string]inventoryBinding{}}
		if err := b.writeInventory(inventory); err != nil {
			return err
		}
		b.inventory, b.inventoryRequired = inventory, true
		return nil
	}
	if err != nil {
		return err
	}
	if len(raw) > authorityMaxDataBytes || rejectDuplicateJSONKeys(string(raw)) != nil {
		return fmt.Errorf("%w: authority inventory is corrupt", ErrReservationUnavailable)
	}
	var inventory authorityInventory
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&inventory); err != nil {
		return fmt.Errorf("%w: authority inventory is corrupt", ErrReservationUnavailable)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("%w: authority inventory has trailing data", ErrReservationUnavailable)
	}
	if inventory.Version != 1 || inventory.Node != node || inventory.Scope != b.scope ||
		inventory.LedgerIdentity != b.ledger.identity || inventory.Bindings == nil {
		return fmt.Errorf("%w: authority inventory identity changed", ErrReservationUnavailable)
	}
	for uid, item := range inventory.Bindings {
		if uid == "" || item.Namespace == "" || item.Name == "" || item.Pin.UID == "" || item.Pin.Epoch == "" {
			return fmt.Errorf("%w: authority inventory is incomplete", ErrReservationUnavailable)
		}
	}
	b.inventory, b.inventoryRequired = &inventory, true
	return nil
}

func (b *Broker) writeInventory(inventory *authorityInventory) error {
	raw, err := json.Marshal(inventory)
	if err != nil {
		return err
	}
	if len(raw) > authorityMaxDataBytes {
		return fmt.Errorf("%w: authority inventory exceeds its bound", ErrReservationUnavailable)
	}
	return writeSynced(filepath.Join(b.root, authorityInventoryFile), raw)
}

// recordAuthorityIdentity precedes the local preparation and global CAS.
func (b *Broker) recordAuthorityIdentity(intent Intent, pin AuthorityPin) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.inventoryRequired {
		return nil
	}
	if b.inventory == nil || !b.inventoryVerified || intent.NodeName != b.inventory.Node ||
		intent.BindingUID == "" || intent.BindingNamespace == "" || intent.BindingName == "" || pin.UID == "" || pin.Epoch == "" {
		return fmt.Errorf("%w: authority inventory is not ready", ErrReservationUnavailable)
	}
	item := inventoryBinding{Namespace: intent.BindingNamespace, Name: intent.BindingName, Pin: pin}
	if old, ok := b.inventory.Bindings[intent.BindingUID]; ok {
		if old != item {
			return fmt.Errorf("%w: recorded authority changed", ErrReservationUnavailable)
		}
		return nil
	}
	next := *b.inventory
	next.Bindings = make(map[string]inventoryBinding, len(b.inventory.Bindings)+1)
	for uid, existing := range b.inventory.Bindings {
		next.Bindings[uid] = existing
	}
	next.Bindings[intent.BindingUID] = item
	if err := b.writeInventory(&next); err != nil {
		return err
	}
	b.inventory = &next
	return nil
}

// VerifyAuthorityInventory reads the complete live Binding and authority sets
// before any admission. Unknown, orphaned, or mismatched history holds work.
func (b *Broker) VerifyAuthorityInventory(ctx context.Context, reader ctrlcli.Reader, maps AuthorityConfigMaps, namespace string) error {
	b.mu.Lock()
	if !b.inventoryRequired {
		b.mu.Unlock()
		return nil
	}
	b.inventoryVerified = false
	if b.inventory == nil {
		b.mu.Unlock()
		return fmt.Errorf("%w: authority inventory is missing", ErrReservationUnavailable)
	}
	inventory := *b.inventory
	inventory.Bindings = make(map[string]inventoryBinding, len(b.inventory.Bindings))
	for uid, item := range b.inventory.Bindings {
		inventory.Bindings[uid] = item
	}
	records := make(map[string]GenerationRecord, len(b.records))
	for generation, rec := range b.records {
		records[generation] = *rec
	}
	b.mu.Unlock()
	activeBindings := map[string]bool{}
	for _, rec := range records {
		if rec.BindingUID == "" || rec.State == StateReleased {
			continue
		}
		activeBindings[rec.BindingUID] = true
		item, ok := inventory.Bindings[rec.BindingUID]
		if !ok || item.Namespace != rec.BindingNamespace || item.Name != rec.BindingName ||
			item.Pin.UID != rec.AuthorityUID || item.Pin.Epoch != rec.AuthorityEpoch || rec.NodeName != inventory.Node {
			return fmt.Errorf("%w: local generation is absent from authority inventory", ErrReservationUnavailable)
		}
	}
	var bindings workercore.ModelStoreBindingList
	if err := reader.List(ctx, &bindings); err != nil {
		return reservationReadError("list Bindings", err)
	}
	byUID := map[string]*workercore.ModelStoreBinding{}
	for i := range bindings.Items {
		byUID[string(bindings.Items[i].UID)] = &bindings.Items[i]
	}
	configmaps, err := maps.List(ctx, meta.ListOptions{})
	if err != nil {
		return reservationReadError("list authorities", err)
	}
	authorityNames := make(map[string]bool, len(configmaps.Items))
	for i := range configmaps.Items {
		authorityNames[configmaps.Items[i].Name] = true
	}
	seen := map[string]bool{}
	localEntryBindings := map[string]bool{}
	for i := range configmaps.Items {
		cm := &configmaps.Items[i]
		if !strings.HasPrefix(cm.Name, "model-transfer-resv-") {
			continue
		}
		var doc authorityDocument
		if json.Unmarshal([]byte(cm.Data[authorityDocumentKey]), &doc) != nil || doc.BindingUID == "" {
			return fmt.Errorf("%w: corrupt authority during inventory", ErrReservationUnavailable)
		}
		pin := AuthorityPin{UID: string(cm.UID), Epoch: doc.Epoch}
		if cm.Namespace != namespace || ValidateAuthorityPin(cm, types.UID(doc.BindingUID), pin) != nil {
			return fmt.Errorf("%w: authority inventory mismatch", ErrReservationUnavailable)
		}
		binding := byUID[doc.BindingUID]
		if binding != nil {
			livePin, err := ParseAuthorityPin(binding.Annotations[AuthorityAnnotation])
			if err != nil || livePin != pin {
				return fmt.Errorf("%w: live Binding pin mismatch", ErrReservationUnavailable)
			}
		} else if len(doc.Entries) == 0 && !activeBindings[doc.BindingUID] {
			// The Binding is gone and no node holds an entry in the
			// document: delete only the exact empty version we observed.
			if deleter, ok := maps.(authorityConfigMapDeleter); ok {
				err := deleter.Delete(ctx, cm.Name, meta.DeleteOptions{Preconditions: &meta.Preconditions{UID: &cm.UID, ResourceVersion: &cm.ResourceVersion}})
				if err != nil && !apierrors.IsNotFound(err) {
					// An empty orphan holds no live charge. Report cleanup failure
					// and retry next pass without invalidating serving generations.
					klog.ErrorS(err, "Delete empty model-transfer authority", "namespace", namespace, "name", cm.Name)
				}
			}
		}
		if saved, ok := inventory.Bindings[doc.BindingUID]; ok {
			if saved.Pin != pin || (binding != nil && (saved.Namespace != binding.Namespace || saved.Name != binding.Name)) {
				return fmt.Errorf("%w: recorded Binding identity changed", ErrReservationUnavailable)
			}
			seen[doc.BindingUID] = true
		}
		for _, entry := range doc.Entries {
			if entry.NodeName == "" {
				return fmt.Errorf("%w: authority entry has no node identity", ErrReservationUnavailable)
			}
			if entry.NodeName != inventory.Node || entry.NodeScope != inventory.Scope {
				continue
			}
			localEntryBindings[doc.BindingUID] = true
			if _, ok := inventory.Bindings[doc.BindingUID]; !ok {
				return fmt.Errorf("%w: orphan authority has no recorded pin", ErrReservationUnavailable)
			}
			rec, ok := records[entry.Generation]
			if entry.LedgerIdentity != inventory.LedgerIdentity || !ok || rec.BindingUID != doc.BindingUID ||
				rec.ReservationID != entry.ReservationID || rec.State == StateReleased {
				return fmt.Errorf("%w: authority entry has no matching local owner", ErrReservationUnavailable)
			}
		}
	}
	pruned := map[string]bool{}
	for uid := range inventory.Bindings {
		if !seen[uid] {
			if byUID[uid] != nil || activeBindings[uid] {
				return fmt.Errorf("%w: recorded authority is missing", ErrReservationUnavailable)
			}
			pruned[uid] = true
			continue
		}
		if byUID[uid] == nil && !activeBindings[uid] && !localEntryBindings[uid] {
			pruned[uid] = true
		}
	}
	for _, binding := range bindings.Items {
		if _, pinned := binding.Annotations[AuthorityAnnotation]; pinned {
			if !authorityNames[AuthorityName(binding.UID)] {
				return fmt.Errorf("%w: pinned authority is missing", ErrReservationUnavailable)
			}
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(pruned) > 0 && b.inventory != nil {
		for _, rec := range b.records {
			if rec.BindingUID != "" && rec.State != StateReleased {
				delete(pruned, rec.BindingUID)
			}
		}
		if len(pruned) > 0 {
			next := *b.inventory
			next.Bindings = make(map[string]inventoryBinding, len(b.inventory.Bindings))
			for uid, item := range b.inventory.Bindings {
				if !pruned[uid] {
					next.Bindings[uid] = item
				}
			}
			if err := b.writeInventory(&next); err != nil {
				return err
			}
			b.inventory = &next
		}
	}
	b.inventoryVerified = true
	return nil
}
