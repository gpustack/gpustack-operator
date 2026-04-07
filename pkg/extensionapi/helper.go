package extensionapi

import (
	"context"
	"fmt"

	"github.com/davecgh/go-spew/spew"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/registry/rest"
	genericapiserver "k8s.io/apiserver/pkg/server"
	ctrl "sigs.k8s.io/controller-runtime"
)

type _APIOptions struct {
	ResourceStorage     rest.Storage
	SubResourceStorages map[string]rest.Storage
}

// ExecuteSetup executes the given setup to install the extension API.
func ExecuteSetup(
	ctx context.Context,
	srv *genericapiserver.GenericAPIServer,
	srvScheme *runtime.Scheme,
	srvParameterCodec runtime.ParameterCodec,
	srvCodec serializer.CodecFactory,
	mgr ctrl.Manager,
	setups []Setup,
) error {
	// Setup all handlers.
	apiOpts := make(map[string]map[string]map[string]_APIOptions)
	for i := range setups {
		opts := SetupOptions{Manager: mgr}
		gvr, srs, err := setups[i].SetupHandler(ctx, opts)
		if err != nil {
			return fmt.Errorf("extension api setup: %s: %w", spew.Sdump(setups[i]), err)
		}

		if apiOpts[gvr.Group] == nil {
			apiOpts[gvr.Group] = make(map[string]map[string]_APIOptions)
		}
		if apiOpts[gvr.Group][gvr.Version] == nil {
			apiOpts[gvr.Group][gvr.Version] = make(map[string]_APIOptions)
		}
		apiOpts[gvr.Group][gvr.Version][gvr.Resource] = _APIOptions{
			ResourceStorage:     setups[i],
			SubResourceStorages: srs,
		}
	}

	agis := make([]*genericapiserver.APIGroupInfo, 0, len(apiOpts))
	for _, gn := range sets.List(sets.KeySet(apiOpts)) {
		agi := genericapiserver.NewDefaultAPIGroupInfo(gn, srvScheme, srvParameterCodec, srvCodec)
		for _, vn := range sets.List(sets.KeySet(apiOpts[gn])) {
			if len(apiOpts[gn][vn]) == 0 {
				continue
			}
			agi.VersionedResourcesStorageMap[vn] = make(map[string]rest.Storage)
			for _, rn := range sets.List(sets.KeySet(apiOpts[gn][vn])) {
				igv := schema.GroupVersion{Group: gn, Version: runtime.APIVersionInternal}
				egv := schema.GroupVersion{Group: gn, Version: vn}
				stg := apiOpts[gn][vn][rn].ResourceStorage
				// TODO(thxCode): Remove this hack after we adopt conversion-gen.
				// Register internal version.
				srvScheme.AddKnownTypes(igv, stg.New())
				if v, ok := stg.(rest.Lister); ok {
					srvScheme.AddKnownTypes(igv, v.NewList())
				}
				// Register primary storage.
				agi.VersionedResourcesStorageMap[vn][rn] = stg
				// Register status subresource if existed.
				if _, ok := stg.New().(ObjectWithStatusSubResource); ok {
					pstg := stg.(StatusSubResourceParentStore)
					stg := AsStatusSubResourceStorage(pstg)
					agi.VersionedResourcesStorageMap[vn][rn+"/status"] = stg
				}
				// Register scale subresource if existed.
				if _, ok := stg.New().(ObjectWithScaleSubResource); ok {
					pstg := stg.(ScaleSubResourceParentStore)
					stg := AsScaleSubResourceStorage(pstg)
					agi.VersionedResourcesStorageMap[vn][rn+"/scale"] = stg
				}
				// Register arbitrary subresources if existed.
				{
					srs := apiOpts[gn][vn][rn].SubResourceStorages
					for srn := range srs {
						sstg := srs[srn]
						// Register internal version.
						srvScheme.AddKnownTypes(igv, sstg.New())
						// Register external version.
						srvScheme.AddKnownTypes(egv, sstg.New())
						// Register storage.
						agi.VersionedResourcesStorageMap[vn][rn+"/"+srn] = sstg
						// Register optional getter options.
						if v, ok := sstg.(rest.GetterWithOptions); ok {
							optionsObj, _, _ := v.NewGetOptions()
							if optionsObj != nil {
								// Register internal version.
								srvScheme.AddKnownTypes(igv, optionsObj)
								// Register external version.
								srvScheme.AddKnownTypes(egv, optionsObj)
							}
						}
						// Register optional connector options.
						if v, ok := sstg.(rest.Connecter); ok {
							optionsObj, _, _ := v.NewConnectOptions()
							if optionsObj != nil {
								// Register internal version.
								srvScheme.AddKnownTypes(igv, optionsObj)
								// Register external version.
								srvScheme.AddKnownTypes(egv, optionsObj)
							}
						}
					}
				}
			}
		}
		agis = append(agis, &agi)
	}

	// Install.
	return srv.InstallAPIGroups(agis...)
}
