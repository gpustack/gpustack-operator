package broker

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// ParseAuthorityPin parses the immutable Binding annotation.
func ParseAuthorityPin(value string) (AuthorityPin, error) {
	var pin AuthorityPin
	if err := rejectDuplicateJSONKeys(value); err != nil {
		return pin, fmt.Errorf("%w: invalid authority pin", ErrReservationUnavailable)
	}
	decoder := json.NewDecoder(bytes.NewBufferString(value))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&pin); err != nil || pin.UID == "" || len(pin.Epoch) != 64 || strings.Trim(pin.Epoch, "0123456789abcdef") != "" {
		return AuthorityPin{}, fmt.Errorf("%w: invalid authority pin", ErrReservationUnavailable)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return AuthorityPin{}, fmt.Errorf("%w: invalid authority pin", ErrReservationUnavailable)
	}
	if _, err := hex.DecodeString(pin.Epoch); err != nil {
		return AuthorityPin{}, fmt.Errorf("%w: invalid authority epoch", ErrReservationUnavailable)
	}
	return pin, nil
}

// ValidateAuthorityPin verifies an annotation against the complete authority document.
func ValidateAuthorityPin(cm *core.ConfigMap, bindingUID types.UID, pin AuthorityPin) error {
	if cm == nil || cm.Name != AuthorityName(bindingUID) {
		return fmt.Errorf("%w: authority name mismatch", ErrReservationUnavailable)
	}
	doc, err := decodeAuthority(cm, pin, authorityMaxEntries)
	if err != nil {
		return err
	}
	if doc.BindingUID != string(bindingUID) {
		return fmt.Errorf("%w: authority Binding mismatch", ErrReservationUnavailable)
	}
	return nil
}

// ReadPinnedAuthority returns a Binding's validated authority entries using a
// live reader. Missing, replaced, or corrupt authority state is an error.
func ReadPinnedAuthority(
	ctx context.Context, reader ctrlcli.Reader, operatorNamespace string, binding *workercore.ModelStoreBinding,
) ([]ReservationEntry, error) {
	if reader == nil || binding == nil || binding.UID == "" || binding.DeletionTimestamp != nil || operatorNamespace == "" {
		return nil, fmt.Errorf("%w: Binding identity is unavailable", ErrReservationUnavailable)
	}
	live := &workercore.ModelStoreBinding{}
	if err := reader.Get(ctx, ctrlcli.ObjectKeyFromObject(binding), live); err != nil {
		return nil, reservationReadError("read Binding", err)
	}
	if live.UID != binding.UID || live.DeletionTimestamp != nil || live.Annotations[AuthorityAnnotation] != binding.Annotations[AuthorityAnnotation] {
		return nil, fmt.Errorf("%w: Binding identity or pin changed", ErrReservationUnavailable)
	}
	pin, err := ParseAuthorityPin(live.Annotations[AuthorityAnnotation])
	if err != nil {
		return nil, err
	}
	cm := &core.ConfigMap{}
	if err := reader.Get(ctx, ctrlcli.ObjectKey{Namespace: operatorNamespace, Name: AuthorityName(binding.UID)}, cm); err != nil {
		return nil, reservationReadError("read pinned authority", err)
	}
	if err := ValidateAuthorityPin(cm, binding.UID, pin); err != nil {
		return nil, err
	}
	doc, err := decodeAuthority(cm, pin, authorityMaxEntries)
	if err != nil {
		return nil, err
	}
	return append([]ReservationEntry(nil), doc.Entries...), nil
}

// defaultGrantRegistrations is the registration count a Binding without its
// own registration figure follows: generous, because in that case the
// node's byte ceiling is what bounds the work.
const defaultGrantRegistrations int32 = 4096

// ResolveGrant resolves the grant a Binding's source preparation draws on:
// the Binding's own sourceMemory section where it names a limit, the node's
// configured capacity where it does not. A Binding without a section
// follows the node entirely, so model-transfer tracks NodeModelStore and
// ModelPrefetch without a mandatory Binding edit.
func ResolveGrant(limits Limits, spec *workercore.ModelStoreBindingSourceMemory, bindingUID types.UID) ReservationGrant {
	grant := ReservationGrant{BindingUID: bindingUID}
	if spec != nil {
		if spec.Bytes.Sign() > 0 {
			grant.Bytes = spec.Bytes.Value()
		}
		if spec.Registrations > 0 {
			grant.Registrations = spec.Registrations
		}
		if spec.Sessions > 0 {
			grant.Sessions = spec.Sessions
		}
	}
	if grant.Bytes <= 0 {
		grant.Bytes = max(AlignDown2M(max(limits.PhysicalBytes, limits.MinMemoryBytes)), MinSourceBufferBytes)
	}
	if grant.Registrations <= 0 {
		grant.Registrations = defaultGrantRegistrations
	}
	if grant.Sessions <= 0 {
		grant.Sessions = max(limits.MaxSessions, 1)
	}
	return grant
}

// EnsureReservationAuthority creates only an unpinned authority, then pins
// its actual UID and epoch with a Binding resourceVersion CAS. A pinned
// missing or replaced authority is never recreated. nodeLimits backs a
// Binding that names no sourceMemory section: each grant field the Binding
// leaves unset follows the node's own configured capacity.
func EnsureReservationAuthority(ctx context.Context, client ctrlcli.Client, maps AuthorityConfigMaps,
	operatorNamespace, bindingNamespace, bindingName string, bindingUID types.UID, nodeLimits Limits,
) (*ReservationAuthority, func(context.Context) (ReservationGrant, error), error) {
	if client == nil || maps == nil || operatorNamespace == "" || bindingUID == "" {
		return nil, nil, fmt.Errorf("%w: authority inputs are incomplete", ErrReservationUnavailable)
	}
	key := ctrlcli.ObjectKey{Namespace: bindingNamespace, Name: bindingName}
	var expectedPin AuthorityPin
	readBinding := func(ctx context.Context) (*workercore.ModelStoreBinding, error) {
		binding := &workercore.ModelStoreBinding{}
		if err := client.Get(ctx, key, binding); err != nil {
			return nil, err
		}
		if binding.UID != bindingUID || binding.DeletionTimestamp != nil {
			return nil, fmt.Errorf("%w: Binding identity changed", ErrReservationUnavailable)
		}
		return binding, nil
	}
	readGrant := func(ctx context.Context) (ReservationGrant, error) {
		binding, err := readBinding(ctx)
		if err != nil {
			return ReservationGrant{}, err
		}
		if expectedPin.UID != "" {
			pin, err := ParseAuthorityPin(binding.Annotations[AuthorityAnnotation])
			if err != nil || pin != expectedPin {
				return ReservationGrant{}, fmt.Errorf("%w: Binding pin changed", ErrReservationUnavailable)
			}
		}
		return ResolveGrant(nodeLimits, binding.Spec.SourceMemory, bindingUID), nil
	}
	for attempt := 0; attempt < authorityCASAttempts; attempt++ {
		binding, err := readBinding(ctx)
		if err != nil {
			return nil, nil, reservationReadError("read Binding", err)
		}
		value, pinned := binding.Annotations[AuthorityAnnotation]
		if pinned {
			pin, err := ParseAuthorityPin(value)
			if err != nil {
				return nil, nil, err
			}
			cm, err := maps.Get(ctx, AuthorityName(bindingUID), meta.GetOptions{})
			if err != nil {
				return nil, nil, reservationReadError("read pinned authority", err)
			}
			if cm.Namespace != operatorNamespace {
				return nil, nil, fmt.Errorf("%w: authority namespace mismatch", ErrReservationUnavailable)
			}
			if err := ValidateAuthorityPin(cm, bindingUID, pin); err != nil {
				return nil, nil, err
			}
			expectedPin = pin
			return NewReservationAuthority(maps, operatorNamespace, bindingUID, pin, readGrant), readGrant, nil
		}
		cm, err := maps.Get(ctx, AuthorityName(bindingUID), meta.GetOptions{})
		if apierrors.IsNotFound(err) {
			var bytes [32]byte
			if _, err := rand.Read(bytes[:]); err != nil {
				return nil, nil, err
			}
			doc := authorityDocument{APIVersion: authorityAPIVersion, BindingUID: string(bindingUID), Epoch: hex.EncodeToString(bytes[:])}
			raw, marshalErr := json.Marshal(doc)
			if marshalErr != nil {
				return nil, nil, marshalErr
			}
			cm, err = maps.Create(ctx, &core.ConfigMap{
				ObjectMeta: meta.ObjectMeta{Name: AuthorityName(bindingUID), Namespace: operatorNamespace},
				Data:       map[string]string{authorityDocumentKey: string(raw)},
			}, meta.CreateOptions{})
			if apierrors.IsAlreadyExists(err) {
				continue
			}
		}
		if err != nil {
			return nil, nil, fmt.Errorf("%w: create authority: %w", ErrReservationUnavailable, err)
		}
		if cm.Namespace != operatorNamespace || cm.UID == "" {
			return nil, nil, fmt.Errorf("%w: authority has no server identity", ErrReservationUnavailable)
		}
		var doc authorityDocument
		if err := json.Unmarshal([]byte(cm.Data[authorityDocumentKey]), &doc); err != nil {
			return nil, nil, fmt.Errorf("%w: invalid authority", ErrReservationUnavailable)
		}
		pin := AuthorityPin{UID: string(cm.UID), Epoch: doc.Epoch}
		if err := ValidateAuthorityPin(cm, bindingUID, pin); err != nil {
			return nil, nil, err
		}
		raw, err := json.Marshal(pin)
		if err != nil {
			return nil, nil, err
		}
		if binding.Annotations == nil {
			binding.Annotations = map[string]string{}
		}
		binding.Annotations[AuthorityAnnotation] = string(raw)
		if err := client.Update(ctx, binding); apierrors.IsConflict(err) {
			continue
		} else if err != nil {
			return nil, nil, fmt.Errorf("%w: pin Binding: %w", ErrReservationUnavailable, err)
		}
		expectedPin = pin
		return NewReservationAuthority(maps, operatorNamespace, bindingUID, pin, readGrant), readGrant, nil
	}
	return nil, nil, fmt.Errorf("%w: Binding pin stayed contested", ErrReservationUnavailable)
}

// RecoverReservationAuthorities restores the pinned authorities referenced by
// local records, including those whose Binding has since been deleted.
func (b *Broker) RecoverReservationAuthorities(ctx context.Context, reader ctrlcli.Reader, maps AuthorityConfigMaps, namespace string) error {
	b.mu.Lock()
	var records []GenerationRecord
	for _, rec := range b.records {
		if rec.BindingUID != "" && rec.State != StateReleased {
			records = append(records, *rec)
		}
	}
	b.mu.Unlock()
	seen := map[string]bool{}
	for _, rec := range records {
		if rec.AuthorityUID == "" || rec.AuthorityEpoch == "" || rec.BindingName == "" || rec.BindingNamespace == "" {
			return fmt.Errorf("%w: recorded authority identity is incomplete", ErrReservationUnavailable)
		}
		uid := types.UID(rec.BindingUID)
		pin := AuthorityPin{UID: rec.AuthorityUID, Epoch: rec.AuthorityEpoch}
		cm, err := maps.Get(ctx, AuthorityName(uid), meta.GetOptions{})
		if err != nil {
			return reservationReadError("read recorded authority", err)
		}
		if cm.Namespace != namespace {
			return fmt.Errorf("%w: recorded authority namespace mismatch", ErrReservationUnavailable)
		}
		if err := ValidateAuthorityPin(cm, uid, pin); err != nil {
			return err
		}
		bindingKey := ctrlcli.ObjectKey{Namespace: rec.BindingNamespace, Name: rec.BindingName}
		grant := func(ctx context.Context) (ReservationGrant, error) {
			binding := &workercore.ModelStoreBinding{}
			if err := reader.Get(ctx, bindingKey, binding); err != nil {
				return ReservationGrant{}, err
			}
			if binding.UID != uid || binding.DeletionTimestamp != nil {
				return ReservationGrant{}, fmt.Errorf("%w: Binding is unavailable", ErrReservationUnavailable)
			}
			currentPin, err := ParseAuthorityPin(binding.Annotations[AuthorityAnnotation])
			if err != nil || currentPin != pin {
				return ReservationGrant{}, fmt.Errorf("%w: Binding pin changed", ErrReservationUnavailable)
			}
			return ResolveGrant(b.LimitsSnapshot(), binding.Spec.SourceMemory, uid), nil
		}
		authority := NewReservationAuthority(maps, namespace, uid, pin, grant)
		b.SetReservationAuthority(uid, authority, grant)
		if !seen[rec.BindingUID] {
			entries, err := authority.Adopt(ctx, b.scope, b.ledger.identity)
			if err != nil {
				return err
			}
			if err := b.adoptAuthoritySessions(rec.BindingUID, entries); err != nil {
				return err
			}
			seen[rec.BindingUID] = true
		}
	}
	return nil
}
