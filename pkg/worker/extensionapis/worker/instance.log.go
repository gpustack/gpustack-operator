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
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/registry/rest"
	restcli "k8s.io/client-go/rest"

	worker "gpustack.ai/gpustack/api/worker/v1"
	"gpustack.ai/gpustack/pkg/extensionapi"
	corev1 "gpustack.ai/gpustack/pkg/kubeclients/kubernetes/typed/core/v1"
	"gpustack.ai/gpustack/pkg/system"
)

// InstanceLogHandler handles the "log" subresource of v1.Instance objects.
//
// InstanceLogHandler streams logs from the v1.Instance's backing Kubernetes Pod,
// which is named as the Instance's name.
type InstanceLogHandler struct {
	ClientCfg restcli.Config
}

func newInstanceLogHandler(_ rest.Scoper, _ extensionapi.SetupOptions) *InstanceLogHandler {
	h := &InstanceLogHandler{}

	// Set client.
	h.ClientCfg = system.LoopbackKubeRestConfig.Get()

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

	// Validate.
	var errs field.ErrorList
	if instLogOpts.TailLines != nil && *instLogOpts.TailLines < 0 {
		errs = append(errs, field.Invalid(
			field.NewPath("tailLines"), *instLogOpts.TailLines, "must be greater than or equal to 0"),
		)
	}
	if instLogOpts.LimitBytes != nil && *instLogOpts.LimitBytes < 1 {
		errs = append(errs, field.Invalid(
			field.NewPath("limitBytes"), *instLogOpts.LimitBytes, "must be greater than 0"))
	}
	switch {
	case instLogOpts.SinceSeconds != nil && instLogOpts.SinceTime != nil:
		errs = append(errs, field.Forbidden(
			field.NewPath(""), "at most one of `sinceTime` or `sinceSeconds` may be specified"))
	case instLogOpts.SinceSeconds != nil:
		if *instLogOpts.SinceSeconds < 1 {
			errs = append(errs, field.Invalid(
				field.NewPath("sinceSeconds"), *instLogOpts.SinceSeconds, "must be greater than 0"))
		}
	}
	if len(errs) > 0 {
		return nil, kerrors.NewInvalid(worker.Kind("InstanceLogOptions"), name, errs)
	}

	restCfg := h.ClientCfg
	stream, err := corev1.NewForConfigOrDie(&restCfg).Pods(key.Namespace).
		GetLogs(key.Name, convertPodLogOptionsFromInstanceLogOptions(instLogOpts)).
		Stream(ctx)
	if err != nil {
		return nil, err
	}

	return &InstanceLogStream{ReadCloser: stream, Flush: instLogOpts.Follow}, nil
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
	Flush bool
}

func (s *InstanceLogStream) GetObjectKind() schema.ObjectKind { return schema.EmptyObjectKind }

func (s *InstanceLogStream) DeepCopyObject() runtime.Object { panic("not supported") }

func (s *InstanceLogStream) InputStream(ctx context.Context, apiVersion, accept string) (io.ReadCloser, bool, string, error) {
	return s.ReadCloser, s.Flush, _InstanceLogMIMEType, nil
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
