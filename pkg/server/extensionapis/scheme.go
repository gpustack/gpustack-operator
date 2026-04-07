package extensionapis

import (
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"

	server "gpustack.ai/gpustack/api/server/v1"
	gpustack "gpustack.ai/gpustack/api/v1"
)

var (
	Scheme             = runtime.NewScheme()
	Codecs             = serializer.NewCodecFactory(Scheme)
	ParameterCodec     = runtime.NewParameterCodec(Scheme)
	localSchemeBuilder = runtime.SchemeBuilder{
		server.Install,
		gpustack.Install,
	}
)

var AddToScheme = localSchemeBuilder.AddToScheme

func init() {
	meta.AddToGroupVersion(Scheme, schema.GroupVersion{Version: "v1"})
	utilruntime.Must(AddToScheme(Scheme))
}
