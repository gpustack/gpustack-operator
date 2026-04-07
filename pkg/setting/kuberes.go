package setting

import (
	"context"

	"github.com/google/uuid"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

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
		// Align data.
		for k := range eSec.Data {
			if _, ok := aSec.Data[k]; !ok {
				aSec.Data[k] = eSec.Data[k]
				skip = false
			}
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
