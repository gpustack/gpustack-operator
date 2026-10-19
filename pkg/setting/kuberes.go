package setting

import (
	"context"
	"strings"

	"github.com/google/uuid"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes"
	"gpustack.ai/gpustack/pkg/kubeclientset"
	"gpustack.ai/gpustack/pkg/kubeclientset/review"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/systemname"
)

var (
	// DelegatedSecretNamespace is the delegated Kubernetes Secret namespace for the settings.
	DelegatedSecretNamespace = systemname.NamespaceName

	// DelegatedSecretName is the delegated Kubernetes Secret name for the settings.
	DelegatedSecretName = "gpustack-settings"
)

// DefaultTrackingName is the name of the Setting whose comma-separated value lists the Settings
// that follow the operator's built-in default instead of an administrator's stored value.
//
// The align pass resolves it by name in the settings registry, and the registry registers it
// under this same name. The two must agree: a Setting registered under any other name would be
// matched by neither the read nor the list, and no Setting would be tracked at all.
const DefaultTrackingName = "settings-default-tracking"

// Initialize initializes Kubernetes resources for settings.
//
// Initialize creates the delegated Kubernetes Secret for settings.
func (h Settings) Initialize(ctx context.Context, cli kubernetes.Interface) error {
	err := review.CanDoUpdate(ctx,
		cli.AuthorizationV1().SelfSubjectAccessReviews(),
		review.Simples{
			{
				Group:    core.SchemeGroupVersion.Group,
				Version:  core.SchemeGroupVersion.Version,
				Resource: "secrets",
			},
		},
		review.WithCreateIfNotExisted(),
	)
	if err != nil {
		return err
	}

	secCli := cli.CoreV1().Secrets(DelegatedSecretNamespace)

	eSec := &core.Secret{
		ObjectMeta: meta.ObjectMeta{
			Name: DelegatedSecretName,
		},
		Data: map[string][]byte{},
	}
	eResType := "settings"
	eNotes := map[string]string{}
	for name, setting := range h {
		eSec.Data[name] = []byte(setting.defVal)
		eNotes[name+"-uid"] = uuid.NewString()
	}
	systemmeta.NoteResource(eSec, eResType, eNotes)
	alignFn := func(aSec *core.Secret) (_ *core.Secret, skip bool, err error) {
		skip = true
		// Align delegated info.
		if !systemmeta.EqualResourceTypeAndNotes(eSec, aSec) {
			systemmeta.NoteResource(aSec, eResType, eNotes)
			skip = false
		}
		// Align data. An absent key is backfilled first, before any rule is evaluated, so a
		// newly registered setting with a blank default still appears and a Secret predating
		// the tracking setting gets its key on this same pass.
		for k := range eSec.Data {
			if _, ok := aSec.Data[k]; !ok {
				aSec.Data[k] = eSec.Data[k]
				skip = false
			}
		}
		tracking := parseTrackedSettings(h, h.trackingValue(aSec))
		for name, s := range h {
			stored := string(aSec.Data[name])
			desired, rule := s.desiredValue(tracking)
			if desired == "" || desired == stored {
				// These rules never write a blank value, and an equal value is not an
				// override, so a pass that changes nothing logs nothing.
				continue
			}
			aSec.Data[name] = []byte(desired)
			skip = false
			pairs := []any{"setting", name, "stored", stored, "value", desired, "rule", rule}
			if s.envName != "" {
				pairs = append(pairs, "envName", s.envName)
			}
			klog.InfoS("setting overridden on start", pairs...)
		}
		return aSec, skip, nil
	}

	_, err = kubeclientset.Update(ctx, secCli, eSec,
		kubeclientset.WithUpdateAlign(alignFn),
		kubeclientset.WithCreateIfNotExisted[*core.Secret]())
	if err != nil {
		return err
	}

	return nil
}

// trackingValue returns the tracking list, read from the live Secret and falling back to the
// registered Setting's built-in default when the Secret carries no such key. Reading the Secret
// alone would defer the built-in list to a later start on a Secret that predates the setting.
//
// The setting is registered by the release that introduces it. Until the registry carries it the
// fallback is empty, so nothing is tracked, which is the correct intermediate state rather than a
// failure to report.
func (h Settings) trackingValue(aSec *core.Secret) string {
	if raw, ok := aSec.Data[DefaultTrackingName]; ok {
		return string(raw)
	}
	if s, ok := h.Index(DefaultTrackingName); ok {
		return s.defVal
	}
	return ""
}

// parseTrackedSettings returns the registered settings the raw tracking list names. It splits on a
// comma, trims each segment, drops a segment left empty, a duplicate, the tracking setting's own
// name, and any name the registry does not carry, so a stored value cannot track a setting that
// does not exist. Every dropped segment is logged once at the fourth verbosity with its reason.
func parseTrackedSettings(h Settings, raw string) sets.Set[string] {
	tracked := sets.New[string]()
	for _, segment := range strings.Split(raw, ",") {
		name := strings.TrimSpace(segment)
		reason := ""
		switch {
		case name == "":
			reason = "blank"
		case tracked.Has(name):
			reason = "duplicate"
		case name == DefaultTrackingName:
			reason = "self-reference"
		default:
			if _, ok := h.Index(name); !ok {
				reason = "not registered"
			}
		}
		if reason != "" {
			klog.V(4).InfoS("tracked setting segment dropped",
				"setting", DefaultTrackingName, "segment", segment, "reason", reason)
			continue
		}
		tracked.Insert(name)
	}
	return tracked
}

// desiredValue returns the value the operator owns for this Setting and the rule that claimed it.
// An empty rule means the operator owns nothing here and the stored value is the administrator's.
//
// The operator writes a Setting when its seed is non-blank and the environment declares it or the
// tracking list names it. A variable present with an empty value yields a blank seed, because a
// built-in default is substituted only for an absent variable, so a blank declaration claims
// nothing even for a Setting that has a default.
func (s Setting) desiredValue(tracked sets.Set[string]) (value, rule string) {
	if isBlank(s.defVal) {
		return "", ""
	}
	if s.declared {
		return s.defVal, "env-declared"
	}
	if tracked.Has(s.name) {
		return s.defVal, "default-tracked"
	}
	return "", ""
}
