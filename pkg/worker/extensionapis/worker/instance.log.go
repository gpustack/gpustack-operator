package worker

import (
	"context"
	"errors"
	"io"
	"net/http"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/registry/rest"

	worker "gpustack.ai/gpustack/api/worker/v1"
	"gpustack.ai/gpustack/pkg/extensionapi"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes"
	"gpustack.ai/gpustack/pkg/system"
)

// InstanceLogHandler handles log request for an instance.
//
// InstanceLogHandler proxies the corresponding Kubernetes Pod resource,
// which is named as the Instance's name.
type InstanceLogHandler struct {
	Client kubernetes.Interface
}

func newInstanceLogHandler(_ rest.Scoper, _ extensionapi.SetupOptions) *InstanceLogHandler {
	h := &InstanceLogHandler{}

	// Set client.
	h.Client = system.LoopbackKubeClient.Get()

	return h
}

var (
	_ rest.Storage           = (*InstanceLogHandler)(nil)
	_ rest.GetterWithOptions = (*InstanceLogHandler)(nil)
)

func (h *InstanceLogHandler) New() runtime.Object {
	return &worker.InstanceLog{}
}

func (h *InstanceLogHandler) Destroy() {
}

func (h *InstanceLogHandler) Get(ctx context.Context, name string, opts runtime.Object) (runtime.Object, error) {
	key, err := extensionapi.KeyFuncForNamespacedScope(ctx, name)
	if err != nil {
		return nil, err
	}

	instLogOpts, ok := opts.(*worker.InstanceLogOptions)
	if !ok {
		return nil, kerrors.NewInternalError(errors.New("invalid options type"))
	}

	stream, err := h.Client.CoreV1().Pods(key.Namespace).
		GetLogs(key.Name, convertPodLogOptionsFromInstanceLogOptions(instLogOpts)).
		Stream(ctx)
	if err != nil {
		return nil, err
	}

	return &InstanceLogStream{ReadCloser: stream}, nil
}

func (h *InstanceLogHandler) NewGetOptions() (runtime.Object, bool, string) {
	return &worker.InstanceLogOptions{}, false, ""
}

const (
	_InstanceLogMIMEType = "text/plain"
)

func (h *InstanceLogHandler) ProducesMIMETypes(verb string) []string {
	return []string{
		_InstanceLogMIMEType,
	}
}

func (h *InstanceLogHandler) ProducesObject(verb string) any {
	return ""
}

func (h *InstanceHandler) OverrideMetricsVerb(verb string) string {
	if verb == http.MethodGet {
		return http.MethodConnect
	}
	return verb
}

type InstanceLogStream struct {
	io.ReadCloser
}

func (s *InstanceLogStream) GetObjectKind() schema.ObjectKind { return schema.EmptyObjectKind }

func (s *InstanceLogStream) DeepCopyObject() runtime.Object { panic("not supported") }

func (s *InstanceLogStream) InputStream(ctx context.Context, apiVersion, accept string) (io.ReadCloser, bool, string, error) {
	return s.ReadCloser, false, _InstanceLogMIMEType, nil
}

func convertPodLogOptionsFromInstanceLogOptions(opts *worker.InstanceLogOptions) *core.PodLogOptions {
	return &core.PodLogOptions{
		Container:    "main",
		Follow:       opts.Follow,
		Timestamps:   opts.Timestamps,
		SinceSeconds: opts.SinceSeconds,
		SinceTime:    opts.SinceTime,
		TailLines:    opts.TailLines,
		LimitBytes:   opts.LimitBytes,
	}
}
