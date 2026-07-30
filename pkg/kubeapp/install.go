package kubeapp

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"

	"golang.org/x/exp/maps"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/rest"
	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/pkg/kubeapp/helm"
	"gpustack.ai/gpustack/pkg/utils/gox"
	"gpustack.ai/gpustack/pkg/utils/osx"
	"gpustack.ai/gpustack/pkg/utils/stringx"
)

// Install is the function type for installing an application. Its last argument reports
// whether the caller has excluded every other process from the release for the whole call.
type Install func(context.Context, *helm.Client, map[string]any, sets.Set[string], bool) error

// ExecuteInstall executes the given installers concurrently, and waits for all of them to complete.
//
// exclusive declares that no other process can be acting on these releases meanwhile. It
// travels to each installer, which is where it decides whether a release left mid-operation
// is repairable or has to be waited out.
func ExecuteInstall(
	ctx context.Context,
	restCfg rest.Config,
	disable sets.Set[string],
	namespace string,
	installs []Install,
	globalValuesContext map[string]any,
	exclusive bool,
) error {
	if disable.Has("*") {
		klog.Warningf("!!! all system applications are disabled !!!")
		return nil
	}

	hc, err := helm.NewClient(restCfg, helm.WithDefaultNamespace(namespace))
	if err != nil {
		return fmt.Errorf("create helm client: %w", err)
	}

	// Append common values to globalValuesContext if not exist,
	// which will be passed to all installers.
	if _, ok := globalValuesContext["Env"]; !ok {
		globalValuesContext["Env"] = getCommonEnv(ctx)
	}
	if _, ok := globalValuesContext["ManagedLabel"]; !ok {
		globalValuesContext["ManagedLabel"] = "gpustack.ai/managed"
	}

	// Execute installers concurrently.
	gp := gox.Group()
	for i := range installs {
		gvc := maps.Clone(globalValuesContext)
		install := installs[i]
		gp.Go(func() error {
			err := install(ctx, hc, gvc, disable, exclusive)
			if err != nil {
				return fmt.Errorf("%s: %w", loadInstallerName(install), err)
			}
			return nil
		})
	}
	return gp.Wait()
}

func getCommonEnv(_ context.Context) (env []core.EnvVar) {
	if v := osx.Getenv("ALL_PROXY"); v != "" {
		env = append(env, core.EnvVar{
			Name:  "ALL_PROXY",
			Value: v,
		})
	}

	if v := osx.Getenv("HTTP_PROXY"); v != "" {
		env = append(env, core.EnvVar{
			Name:  "HTTP_PROXY",
			Value: v,
		})
	}

	if v := osx.Getenv("HTTPS_PROXY"); v != "" {
		env = append(env, core.EnvVar{
			Name:  "HTTPS_PROXY",
			Value: v,
		})
	}

	if v := osx.Getenv("NO_PROXY"); v != "" {
		env = append(env, core.EnvVar{
			Name:  "NO_PROXY",
			Value: v,
		})
	}

	return env
}

func loadInstallerName(i Install) string {
	n := runtime.FuncForPC(reflect.ValueOf(i).Pointer()).Name()
	n = strings.TrimPrefix(strings.TrimSuffix(filepath.Ext(n), "-fm"), ".")
	return stringx.Decamelize(n, true)
}
