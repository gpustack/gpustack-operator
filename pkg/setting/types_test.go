package setting

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"gpustack.ai/gpustack/pkg/system"
)

func TestInitializerDeclarationSource(t *testing.T) {
	const (
		derivedName   = "GPUSTACK_TEST_SETTING"
		specifiedName = "TEST_SPECIFIED_SETTING"
	)

	type testCase struct {
		name         string
		envName      string
		envValue     *string
		initializer  func() Initializer
		wantValue    string
		wantEnvName  string
		wantDeclared bool
	}

	declaredValue := "configured"
	emptyValue := ""
	cases := []testCase{
		{
			name:         "environment declared",
			envName:      derivedName,
			envValue:     &declaredValue,
			initializer:  func() Initializer { return InitializeFromEnv("fallback") },
			wantValue:    declaredValue,
			wantEnvName:  derivedName,
			wantDeclared: true,
		},
		{
			name:        "environment unset",
			envName:     derivedName,
			initializer: func() Initializer { return InitializeFromEnv("fallback") },
			wantValue:   "fallback",
			wantEnvName: derivedName,
		},
		{
			name:         "environment declared empty",
			envName:      derivedName,
			envValue:     &emptyValue,
			initializer:  func() Initializer { return InitializeFromEnv("fallback") },
			wantEnvName:  derivedName,
			wantDeclared: true,
		},
		{
			name:         "specified environment declared",
			envName:      specifiedName,
			envValue:     &declaredValue,
			initializer:  func() Initializer { return InitializeFromSpecifiedEnv(specifiedName, "fallback") },
			wantValue:    declaredValue,
			wantEnvName:  specifiedName,
			wantDeclared: true,
		},
		{
			name:        "fixed value",
			initializer: func() Initializer { return InitializeFrom("fixed") },
			wantValue:   "fixed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.envValue == nil {
				require.NoError(t, os.Unsetenv(tc.envName))
			} else {
				t.Setenv(tc.envName, *tc.envValue)
			}

			s := Settings{}.New("test-setting", "", PropPrivate, tc.initializer())
			assert.Equal(t, tc.wantValue, s.DefaultValue())
			assert.Equal(t, tc.wantEnvName, s.envName)
			assert.Equal(t, tc.wantDeclared, s.declared)
		})
	}
}

// TestSettingValueBool pins what a bool Setting reads as. A read that fails, or finds no stored
// value, yields the setting's default together with the error, so ShouldValueBool, which drops the
// error, still returns the default: a setting that defaults to on must not turn off because one read
// failed.
func TestSettingValueBool(t *testing.T) {
	const name = "test-bool"

	cases := []struct {
		name     string
		defVal   string
		stored   *string // nil stores no value for the setting; a failed read never reaches it
		readFail bool
		want     bool
		wantErr  bool
	}{
		{name: "default true, read fails", defVal: "true", stored: ptr.To("false"), readFail: true, want: true, wantErr: true},
		{name: "default true, no stored value", defVal: "true", want: true, wantErr: true},
		{name: "default false, read fails", defVal: "false", stored: ptr.To("true"), readFail: true, want: false, wantErr: true},
		{name: "stored false under default true", defVal: "true", stored: ptr.To("false"), want: false},
		{name: "stored true under default false", defVal: "false", stored: ptr.To("true"), want: true},
		{name: "stored value is not a bool", defVal: "true", stored: ptr.To("maybe"), want: false, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			seedDelegatedSecret(t, name, c.stored)
			if c.readFail {
				errGet = errors.New("injected read failure")
				t.Cleanup(func() { errGet = nil })
			}
			s := Settings{}.New(name, "", PropPrivate, InitializeFrom(c.defVal), AllowBool())

			got, err := s.ValueBool(context.Background())
			assert.Equal(t, c.want, got, "ValueBool")
			if c.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
			assert.Equal(t, c.want, s.ShouldValueBool(context.Background()), "ShouldValueBool")
		})
	}
}

// seedDelegatedSecret recreates the delegated Secret holding only the given value for name, or no
// value when stored is nil, and flushes the cache so the next read reaches it.
func seedDelegatedSecret(t *testing.T, name string, stored *string) {
	t.Helper()

	ctx := context.Background()
	cli := system.LoopbackCtrlClient.Get()
	sec := &core.Secret{ObjectMeta: meta.ObjectMeta{Namespace: DelegatedSecretNamespace, Name: DelegatedSecretName}}
	_ = cli.Delete(ctx, sec)
	sec.Data = map[string][]byte{"another-setting": []byte("true")}
	if stored != nil {
		sec.Data[name] = []byte(*stored)
	}
	require.NoError(t, cli.Create(ctx, sec))
	InvalidateCache()
	t.Cleanup(InvalidateCache)
}
