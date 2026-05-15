package kubediscovery

import (
	"context"
	"fmt"
	"strings"
	"time"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/pkg/utils/waitx"
)

// Lookup looks up an object in the cluster by its API version, kind, namespace, and name,
// and returns the object as a map[string]any.
//
// If the object does not exist, it returns an empty map and no error.
//
// This function supports both object and list result.
func Lookup(
	ctx context.Context,
	discoveryCli discovery.DiscoveryInterface,
	apiversion, kind, namespace, name string,
) (map[string]any, error) {
	gvk := schema.FromAPIVersionAndKind(apiversion, kind)

	dynNrCli, namespaced, err := GetDynamicNamespaceableResourceClientForGVK(discoveryCli, gvk)
	if err != nil {
		return nil, err
	}

	var dynCli dynamic.ResourceInterface
	if namespaced && namespace != "" {
		dynCli = dynNrCli.Namespace(namespace)
	} else {
		dynCli = dynNrCli
	}

	if name != "" {
		obj, err := dynCli.Get(ctx, name,
			meta.GetOptions{})
		if err != nil {
			if kerrors.IsNotFound(err) {
				return map[string]any{}, nil
			}
			return nil, err
		}
		return obj.UnstructuredContent(), nil
	}

	objList, err := dynCli.List(ctx,
		meta.ListOptions{})
	if err != nil {
		if kerrors.IsNotFound(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	return objList.UnstructuredContent(), nil
}

// LookupObject looks up an object in the cluster by its API version, kind, namespace, and name.
//
// If the object does not exist, it returns an empty unstructured.Unstructured and no error.
func LookupObject(
	ctx context.Context,
	discoveryCli discovery.DiscoveryInterface,
	apiversion, kind, namespace, name string,
) (*unstructured.Unstructured, error) {
	gvk := schema.FromAPIVersionAndKind(apiversion, kind)

	dynNrCli, namespaced, err := GetDynamicNamespaceableResourceClientForGVK(discoveryCli, gvk)
	if err != nil {
		return nil, err
	}

	var dynCli dynamic.ResourceInterface
	if namespaced && namespace != "" {
		dynCli = dynNrCli.Namespace(namespace)
	} else {
		dynCli = dynNrCli
	}

	obj, err := dynCli.Get(ctx, name,
		meta.GetOptions{})
	if err != nil {
		if kerrors.IsNotFound(err) {
			return &unstructured.Unstructured{}, nil
		}
		return nil, err
	}
	return obj, nil
}

// LookupList looks up a list of objects in the cluster by their API version, kind, and namespace.
//
// If the objects do not exist, it returns an empty unstructured.UnstructuredList and no error.
func LookupList(
	ctx context.Context,
	discoveryCli discovery.DiscoveryInterface,
	apiversion, kind, namespace string,
) (*unstructured.UnstructuredList, error) {
	gvk := schema.FromAPIVersionAndKind(apiversion, kind)

	dynNrCli, namespaced, err := GetDynamicNamespaceableResourceClientForGVK(discoveryCli, gvk)
	if err != nil {
		return nil, err
	}

	var dynCli dynamic.ResourceInterface
	if namespaced && namespace != "" {
		dynCli = dynNrCli.Namespace(namespace)
	} else {
		dynCli = dynNrCli
	}

	objList, err := dynCli.List(ctx,
		meta.ListOptions{})
	if err != nil {
		if kerrors.IsNotFound(err) {
			return &unstructured.UnstructuredList{}, nil
		}
		return nil, err
	}
	return objList, nil
}

// GetDynamicNamespaceableResourceClientForGVK returns a dynamic client for the given GroupVersionKind,
// as well as a boolean indicating whether the resources are namespaced.
func GetDynamicNamespaceableResourceClientForGVK(
	discoveryCli discovery.DiscoveryInterface,
	gvk schema.GroupVersionKind,
) (dynamic.NamespaceableResourceInterface, bool, error) {
	apiRes, err := GetAPIResourceForGVK(discoveryCli, gvk)
	if err != nil {
		return nil, false, err
	}

	cli := dynamic.New(discoveryCli.RESTClient())
	resCli := cli.Resource(schema.GroupVersionResource{
		Group:    apiRes.Group,
		Version:  apiRes.Version,
		Resource: apiRes.Name,
	})
	return resCli, apiRes.Namespaced, nil
}

// GetAPIResourceForGVK returns the APIResource for the given GroupVersionKind.
func GetAPIResourceForGVK(
	discoveryCli discovery.DiscoveryInterface,
	gvk schema.GroupVersionKind,
) (meta.APIResource, error) {
	var apiResource meta.APIResource

	list, err := discoveryCli.ServerResourcesForGroupVersion(gvk.GroupVersion().String())
	if err != nil {
		return apiResource, err
	}

	for _, resource := range list.APIResources {
		if resource.Kind == gvk.Kind && !strings.Contains(resource.Name, "/") {
			apiResource = resource
			apiResource.Group = gvk.Group
			apiResource.Version = gvk.Version
			break
		}
	}
	if apiResource.Name == "" {
		return apiResource, fmt.Errorf("APIResource not found for GVK: %v", gvk)
	}

	return apiResource, nil
}

// WaitUntilConnected waits until the Kubernetes to be connected,
// or returns an error if the context is canceled.
func WaitUntilConnected(ctx context.Context, discoveryCli discovery.DiscoveryInterface) error {
	return waitx.PollUntilContextCancel(ctx, time.Second, true,
		func(ctx context.Context) error {
			err := IsConnected(ctx, discoveryCli)
			if err != nil {
				klog.V(3).ErrorS(err, "connect to Kubernetes, retrying...")
			}
			return err
		},
	)
}

// WaitUntilConnectedWithRestConfig is similar to WaitUntilConnected,
// accepts rest.Config as input.
func WaitUntilConnectedWithRestConfig(ctx context.Context, cfg *rest.Config) error {
	return waitx.PollUntilContextCancel(ctx, time.Second, true,
		func(ctx context.Context) error {
			err := IsConnectedWithRestConfig(ctx, cfg)
			if err != nil {
				klog.V(3).ErrorS(err, "connect to Kubernetes, retrying...")
			}
			return err
		},
	)
}

// IsConnected returns nothing if the Kubernetes cluster is connected.
func IsConnected(ctx context.Context, discoveryCli discovery.DiscoveryInterface) error {
	_, err := GetVersion(ctx, discoveryCli)
	return err
}

// IsConnectedWithRestConfig is similar to IsConnected, accepts rest.Config as input.
func IsConnectedWithRestConfig(ctx context.Context, restCfg *rest.Config) error {
	_, err := GetVersionWithRestConfig(ctx, restCfg)
	return err
}

// Version represents the Kubernetes version information.
type Version = version.Info

// GetVersion returns the Kubernetes version information.
func GetVersion(_ context.Context, discoveryCli discovery.DiscoveryInterface) (*Version, error) {
	info, err := discoveryCli.ServerVersion()
	if err != nil {
		return nil, fmt.Errorf("get server version: %w", err)
	}

	return info, nil
}

// GetVersionWithRestConfig returns the Kubernetes version information with the given REST config.
func GetVersionWithRestConfig(ctx context.Context, restCfg *rest.Config) (*Version, error) {
	discoveryCli, err := discovery.NewDiscoveryClientForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("create discovery client: %w", err)
	}

	return GetVersion(ctx, discoveryCli)
}
