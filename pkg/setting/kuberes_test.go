package setting

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authz "k8s.io/api/authorization/v1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	k8stesting "k8s.io/client-go/testing"

	kubefake "gpustack.ai/gpustack/pkg/kubeclients/kubernetes/fake"
)

type (
	// caseSetting declares one Setting a case registers. A non-empty envName registers it
	// through that environment variable, and such a case must also declare that variable,
	// because a Setting's declaration is fixed when it is registered.
	caseSetting struct {
		name    string
		defVal  string
		envName string
	}

	// initializeCase is one row: the registry it builds, the environment it declares, the
	// Secret that already exists and the values it must hold afterwards. A nil stored map
	// means no Secret exists, so Initialize takes the creation path; a non-nil map means the
	// Secret exists holding exactly those keys, and a key the registry also carries is
	// already present rather than backfilled.
	initializeCase struct {
		name     string
		settings []caseSetting
		env      map[string]string
		stored   map[string]string
		want     map[string]string
	}
)

// runInitializeCases runs each row through one execution loop: declare the environment, build
// the registry, run Initialize against a fake clientset, then assert on the Secret the clientset
// holds. It must not be called from a parallel test, because a row declares its environment.
func runInitializeCases(t *testing.T, cases []initializeCase) {
	t.Helper()

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for name, value := range c.env {
				t.Setenv(name, value)
			}
			cli := kubefake.NewSimpleClientset(delegatedSecretObjects(c.stored)...)
			allowSelfSubjectAccessReviews(cli)

			require.NoError(t, registerCaseSettings(c.settings).
				Initialize(context.Background(), cli))

			sec, err := cli.CoreV1().Secrets(DelegatedSecretNamespace).
				Get(context.Background(), DelegatedSecretName, meta.GetOptions{})
			require.NoError(t, err)
			for name, want := range c.want {
				got, ok := sec.Data[name]
				assert.True(t, ok, "setting %s is present", name)
				assert.Equal(t, want, string(got), "setting %s", name)
			}
		})
	}
}

// registerCaseSettings builds the registry a row declares.
func registerCaseSettings(declared []caseSetting) Settings {
	h := Settings{}
	for _, s := range declared {
		if s.envName != "" {
			h.NewEditable(s.name, "A setting registered by a case.",
				InitializeFromSpecifiedEnv(s.envName, s.defVal))
			continue
		}
		h.NewEditable(s.name, "A setting registered by a case.", InitializeFrom(s.defVal))
	}
	return h
}

// delegatedSecretObjects returns the objects a fake clientset starts with, so that a row either
// finds no Secret at all or finds one holding exactly the given keys.
func delegatedSecretObjects(stored map[string]string) []runtime.Object {
	if stored == nil {
		return nil
	}
	data := map[string][]byte{}
	for name, value := range stored {
		data[name] = []byte(value)
	}
	return []runtime.Object{&core.Secret{
		ObjectMeta: meta.ObjectMeta{
			Namespace: DelegatedSecretNamespace,
			Name:      DelegatedSecretName,
		},
		Data: data,
	}}
}

// allowSelfSubjectAccessReviews makes the permission check Initialize runs first pass, which a
// fake client denies by default.
func allowSelfSubjectAccessReviews(cli *kubefake.Clientset) {
	cli.PrependReactor("create", "selfsubjectaccessreviews",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			sar, ok := action.(k8stesting.CreateAction).GetObject().(*authz.SelfSubjectAccessReview)
			if !ok {
				return false, nil, nil
			}
			sar = sar.DeepCopy()
			sar.Status.Allowed = true
			return true, sar, nil
		})
}

const (
	trackedImage = "kv-cache-backend-image"
	blankImage   = "container-registry"
)

// TestSettingsInitialize_DeclaredSettingOverridesStoredValue pins that a variable declared in the
// environment is a standing desired state rather than a one-time default. Removing the rules that
// claim a declared value leaves this row alone on its stored value.
func TestSettingsInitialize_DeclaredSettingOverridesStoredValue(t *testing.T) {
	const (
		name    = "instance-access-static-address"
		envName = "GPUSTACK_INSTANCE_ACCESS_STATIC_ADDRESS"
	)
	runInitializeCases(t, []initializeCase{
		{
			name:     "a declared variable overrides the stored value of an untracked setting",
			settings: []caseSetting{{name: name, envName: envName}},
			env:      map[string]string{envName: "new.example"},
			stored:   map[string]string{name: "old.example"},
			want:     map[string]string{name: "new.example"},
		},
	})
}

// TestSettingsInitialize_TrackedSettingFollowsBuiltInDefault pins that a Setting the tracking list
// names follows the operator's default, so a new default reaches a cluster on the next start.
func TestSettingsInitialize_TrackedSettingFollowsBuiltInDefault(t *testing.T) {
	runInitializeCases(t, []initializeCase{
		{
			name: "a tracked setting follows the built-in default",
			settings: []caseSetting{
				{name: trackedImage, defVal: "operator-default/image:v1"},
				{name: DefaultTrackingName, defVal: trackedImage},
			},
			stored: map[string]string{trackedImage: "my.pin/image:1.0", DefaultTrackingName: trackedImage},
			want:   map[string]string{trackedImage: "operator-default/image:v1", DefaultTrackingName: trackedImage},
		},
	})
}

// TestSettingsInitialize_UntrackedStoredValueIsLeftAlone pins that a value the operator owns
// nothing over stays the administrator's, a deliberate blank included.
func TestSettingsInitialize_UntrackedStoredValueIsLeftAlone(t *testing.T) {
	runInitializeCases(t, []initializeCase{
		{
			name:     "a non-blank administrator value is kept",
			settings: []caseSetting{{name: trackedImage, defVal: "operator-default/image:v1"}},
			stored:   map[string]string{trackedImage: "my.pin/image:1.0"},
			want:     map[string]string{trackedImage: "my.pin/image:1.0"},
		},
		{
			name:     "a blank administrator value is kept",
			settings: []caseSetting{{name: trackedImage, defVal: "operator-default/image:v1"}},
			stored:   map[string]string{trackedImage: ""},
			want:     map[string]string{trackedImage: ""},
		},
	})
}

// TestSettingsInitialize_BackfillsAbsentKeysIncludingBlanks pins that the align pass still adds
// every registered key the Secret lacks, so a newly registered setting with a blank default
// appears instead of being missing.
func TestSettingsInitialize_BackfillsAbsentKeysIncludingBlanks(t *testing.T) {
	runInitializeCases(t, []initializeCase{
		{
			name: "a blank default is a key the Secret is given",
			settings: []caseSetting{
				{name: trackedImage, defVal: "operator-default/image:v1"},
				{name: blankImage, defVal: ""},
			},
			stored: map[string]string{"another-setting": "kept"},
			want: map[string]string{
				trackedImage:      "operator-default/image:v1",
				blankImage:        "",
				"another-setting": "kept",
			},
		},
	})
}

// TestSettingsInitialize_TrackingFallsBackToDefaultWhenKeyAbsent pins that the built-in tracking
// list applies on the same pass that backfills its key, so a Secret predating the setting does
// not defer the transition to a later start.
func TestSettingsInitialize_TrackingFallsBackToDefaultWhenKeyAbsent(t *testing.T) {
	runInitializeCases(t, []initializeCase{
		{
			name: "the built-in list applies when the Secret has no tracking key",
			settings: []caseSetting{
				{name: trackedImage, defVal: "operator-default/image:v1"},
				{name: DefaultTrackingName, defVal: trackedImage},
			},
			stored: map[string]string{trackedImage: "my.pin/image:1.0"},
			want: map[string]string{
				trackedImage:        "operator-default/image:v1",
				DefaultTrackingName: trackedImage,
			},
		},
	})
}

// TestSettingsInitialize_EqualValueIsNotAnOverride pins that a stored value equal to what the
// operator would write is not an override, so a quiet start logs nothing.
func TestSettingsInitialize_EqualValueIsNotAnOverride(t *testing.T) {
	const envName = "GPUSTACK_TEST_IMAGE"
	runInitializeCases(t, []initializeCase{
		{
			name:     "a declared variable equal to the stored value leaves it alone",
			settings: []caseSetting{{name: trackedImage, defVal: "operator-default/image:v1", envName: envName}},
			env:      map[string]string{envName: "operator-default/image:v1"},
			stored:   map[string]string{trackedImage: "operator-default/image:v1"},
			want:     map[string]string{trackedImage: "operator-default/image:v1"},
		},
		{
			name: "a tracked setting already holding its default is left alone",
			settings: []caseSetting{
				{name: trackedImage, defVal: "operator-default/image:v1"},
				{name: DefaultTrackingName, defVal: trackedImage},
			},
			stored: map[string]string{trackedImage: "operator-default/image:v1", DefaultTrackingName: trackedImage},
			want:   map[string]string{trackedImage: "operator-default/image:v1"},
		},
	})
}

// TestSettingsInitialize_BlankDeclarationReadsAsNoDeclaration pins that a variable present with
// an empty value claims nothing, even for a Setting that has a built-in default to fall back on:
// a default is substituted only for an absent variable, so the seed here is blank.
func TestSettingsInitialize_BlankDeclarationReadsAsNoDeclaration(t *testing.T) {
	const envName = "GPUSTACK_TEST_IMAGE"
	runInitializeCases(t, []initializeCase{
		{
			name:     "an empty declaration leaves an untracked setting alone",
			settings: []caseSetting{{name: trackedImage, defVal: "operator-default/image:v1", envName: envName}},
			env:      map[string]string{envName: ""},
			stored:   map[string]string{trackedImage: "my.pin/image:1.0"},
			want:     map[string]string{trackedImage: "my.pin/image:1.0"},
		},
	})
}

// TestSettingsInitialize_CreationSeedsEverySetting pins that a cluster with no delegated Secret
// gets one carrying every registered key, blank defaults included.
func TestSettingsInitialize_CreationSeedsEverySetting(t *testing.T) {
	runInitializeCases(t, []initializeCase{
		{
			name: "an absent Secret is created carrying every registered key",
			settings: []caseSetting{
				{name: trackedImage, defVal: "operator-default/image:v1"},
				{name: blankImage, defVal: ""},
			},
			want: map[string]string{
				trackedImage: "operator-default/image:v1",
				blankImage:   "",
			},
		},
	})
}

// TestParseTrackedSettings pins the normalization a stored tracking value goes through, so a value
// an administrator writes cannot silently change which settings are tracked.
func TestParseTrackedSettings(t *testing.T) {
	const (
		alpha = "alpha-image"
		beta  = "beta-image"
	)
	h := Settings{}
	h.NewEditable(alpha, "A setting registered by a case.", InitializeFrom("a"))
	h.NewEditable(beta, "A setting registered by a case.", InitializeFrom("b"))
	h.NewEditable(DefaultTrackingName, "A setting registered by a case.", InitializeFrom(alpha))

	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{
			name: "surrounding whitespace is trimmed",
			raw:  "  " + alpha + " , " + beta + "  ",
			want: []string{alpha, beta},
		},
		{
			name: "a segment empty after trimming is dropped",
			raw:  alpha + ", , " + beta,
			want: []string{alpha, beta},
		},
		{
			name: "a duplicate is dropped",
			raw:  alpha + ", " + alpha,
			want: []string{alpha},
		},
		{
			name: "the tracking setting's own name is dropped",
			raw:  DefaultTrackingName,
			want: []string{},
		},
		{
			name: "a name the registry does not carry is dropped",
			raw:  alpha + ",retired-image",
			want: []string{alpha},
		},
		{
			name: "an empty value tracks nothing",
			raw:  "",
			want: []string{},
		},
		{
			name: "every step applies together",
			raw:  " " + alpha + " , " + DefaultTrackingName + " , retired-image , " + alpha + " , ",
			want: []string{alpha},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, sets.List(parseTrackedSettings(h, c.raw)))
		})
	}
}
