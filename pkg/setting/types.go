package setting

import (
	"context"
	"fmt"
	"strconv"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	"gpustack.ai/gpustack/pkg/kubeclientset"
	"gpustack.ai/gpustack/pkg/system"
	"gpustack.ai/gpustack/pkg/utils/funcx"
)

// Prop represents the properties of a setting, which can be a combination of editable, private and sensitive.
type Prop uint8

const (
	PropEditable Prop = 1 << (iota)
	PropPrivate
	PropSensitive
)

type (
	// Setting represents a server setting, which has a name, description, default value, admission function and properties.
	Setting struct {
		name        string
		description string
		defVal      string
		admit       Admission
		props       Prop
	}

	// IndexFunc is a function type that takes a setting name and returns the corresponding setting and a boolean indicating whether the setting exists.
	IndexFunc = func(name string) (Setting, bool)
)

// Name returns the name of the setting.
func (s Setting) Name() string {
	return s.name
}

// Description returns the description of the setting.
func (s Setting) Description() string {
	return s.description
}

func (s Setting) DefaultValue() string {
	return s.defVal
}

// Editable returns true if the setting is editable.
func (s Setting) Editable() bool {
	return s.props&PropEditable == PropEditable
}

// Sensitive returns true if the setting is sensitive.
func (s Setting) Sensitive() bool {
	return s.props&PropSensitive == PropSensitive
}

// Private returns true if the setting is private.
func (s Setting) Private() bool {
	return s.props&PropPrivate == PropPrivate
}

// Configure configures the value of the setting.
func (s Setting) Configure(ctx context.Context, newVal string) error {
	lpCli := system.LoopbackKubeClient.Get()

	// Update.
	eSec := &core.Secret{
		ObjectMeta: meta.ObjectMeta{
			Namespace: DelegatedSecretNamespace,
			Name:      DelegatedSecretName,
		},
	}
	alignFn := func(aSec *core.Secret) (*core.Secret, bool, error) {
		var oldVal string
		if aSec.Data != nil {
			oldVal = string(aSec.Data[s.name])
		}
		if admitErr := s.admit(ctx, oldVal, newVal); admitErr != nil {
			// NB(thxCode): Skip update if the new value is invalid.
			return nil, false, admitErr
		}
		if oldVal == newVal {
			// Skip update if the new value is the same as the old value.
			return nil, true, nil
		}
		// Update the value of the setting.
		aSec.Data[s.name] = []byte(newVal)
		return aSec, false, nil
	}

	secCli := lpCli.CoreV1().Secrets(DelegatedSecretNamespace)
	_, err := kubeclientset.Update(ctx, secCli, eSec,
		kubeclientset.WithUpdateAlign(alignFn))
	if err != nil {
		return fmt.Errorf("configure setting %s: %w", s.name, err)
	}

	return nil
}

// ValueFromRemote returns the value of the setting by directly accessing the delegated secret in Kubernetes API server,
// which is used for remote access and does not involve the controller-runtime client cache.
//
// This is useful for scenarios when the controller-runtime client cache has not been synced yet,
// or when we want to bypass the cache for some reason.
// However, it may have performance implications and should be used with caution.
//
// If the value is not found in the delegated secret, it returns the default value of the setting.
func (s Setting) ValueFromRemote(ctx context.Context) (string, error) {
	lpCli := system.LoopbackKubeClient.Get()

	sec, err := lpCli.CoreV1().
		Secrets(DelegatedSecretNamespace).
		Get(ctx, DelegatedSecretName,
			meta.GetOptions{})
	if err != nil {
		return s.defVal, fmt.Errorf("get value of setting %s: %w", s.name, err)
	}

	if sec.Data == nil || sec.Data[s.name] == nil {
		return s.defVal, fmt.Errorf("get value of setting %s: not found", s.name)
	}

	return string(sec.Data[s.name]), nil
}

// ShouldValueFromRemote returns the value of the setting from remote without error.
func (s Setting) ShouldValueFromRemote(ctx context.Context) string {
	return funcx.NoError(s.ValueFromRemote(ctx))
}

// Value returns the value of the setting.
//
// If the value is not found in the delegated secret, it returns the default value of the setting.
func (s Setting) Value(ctx context.Context) (string, error) {
	lpCli := system.LoopbackCtrlClient.Get()

	sec := &core.Secret{
		ObjectMeta: meta.ObjectMeta{
			Name:      DelegatedSecretName,
			Namespace: DelegatedSecretNamespace,
		},
	}
	err := lpCli.Get(ctx, ctrlcli.ObjectKeyFromObject(sec), sec,
		&ctrlcli.GetOptions{
			Raw: &meta.GetOptions{
				ResourceVersion: "0",
			},
		})
	if err != nil {
		return s.defVal, fmt.Errorf("get value of setting %s: %w", s.name, err)
	}

	if sec.Data == nil || sec.Data[s.name] == nil {
		return s.defVal, fmt.Errorf("get value of setting %s: not found", s.name)
	}

	return string(sec.Data[s.name]), nil
}

// ShouldValue returns the value of the setting without error.
func (s Setting) ShouldValue(ctx context.Context) string {
	return funcx.NoError(s.Value(ctx))
}

// ValueBool returns the boolean value of the setting.
func (s Setting) ValueBool(ctx context.Context) (bool, error) {
	valStr, err := s.Value(ctx)
	if err != nil {
		return false, err
	}
	return strconv.ParseBool(valStr)
}

// ShouldValueBool returns the boolean value of the setting without error.
func (s Setting) ShouldValueBool(ctx context.Context) bool {
	return funcx.NoError(s.ValueBool(ctx))
}

type Settings map[string]Setting

// New creates a new setting with the given name, description, properties, initializer and admissions,
// and registers it in the settings map.
func (h Settings) New(
	name, description string, props Prop,
	init Initializer, admits ...Admission,
) Setting {
	s := Setting{
		name:        name,
		defVal:      init(name),
		admit:       AdmitWith(admits...),
		description: description,
		props:       props,
	}
	h[name] = s
	return s
}

// NewEditable creates a new editable setting with the given name, description, initializer and admissions.
// This setting is editable on UI, and its value can be modified by users, but it is not hidden on UI and logs.
func (h Settings) NewEditable(
	name, description string,
	init Initializer, admits ...Admission,
) Setting {
	return h.New(
		name, description, PropEditable,
		init, admits...,
	)
}

// NewSensitive creates a new sensitive setting with the given name, description, initializer and admissions.
// This setting is editable, but its value is hidden on UI and logs, and it should be handled with extra care.
func (h Settings) NewSensitive( // nolint:unused
	name, description string,
	init Initializer, admits ...Admission,
) Setting {
	return h.New(
		name, description, PropEditable|PropSensitive,
		init, admits...,
	)
}

// NewPrivate creates a new private setting with the given name, description, initializer and admissions.
// This setting is not visible to users, but can be configured and used by the server internally.
func (h Settings) NewPrivate(
	name, description string,
	init Initializer, admits ...Admission,
) Setting {
	return h.New(
		name, description, PropPrivate,
		init, admits...,
	)
}

// Index returns the setting with the given name.
func (h Settings) Index(name string) (Setting, bool) {
	s, ok := h[name]
	return s, ok
}
