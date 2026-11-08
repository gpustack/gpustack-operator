package worker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlinterceptor "sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/modeltransfer/broker"
	"gpustack.ai/gpustack/pkg/setting"
)

func TestAuthorityPinFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		missing, noReader, apiFailure bool
		want                          field.ErrorType
	}{
		{name: "missing document", missing: true, want: field.ErrorTypeForbidden},
		{name: "missing reader", noReader: true, want: field.ErrorTypeInternal},
		{name: "API failure", apiFailure: true, want: field.ErrorTypeInternal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := newModelStoreBinding()
			old.UID = "binding-uid"
			current := old.DeepCopy()
			pin, err := json.Marshal(broker.AuthorityPin{UID: "authority-uid", Epoch: strings.Repeat("a", 64)})
			require.NoError(t, err)
			current.Annotations = map[string]string{broker.AuthorityAnnotation: string(pin)}
			cm := &core.ConfigMap{ObjectMeta: meta.ObjectMeta{Namespace: setting.DelegatedSecretNamespace, Name: broker.AuthorityName(old.UID), UID: "authority-uid"}}
			builder := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme)
			if !tc.missing {
				builder.WithObjects(cm)
			}
			reader := builder.Build()
			if tc.apiFailure {
				reader = ctrlinterceptor.NewClient(reader, ctrlinterceptor.Funcs{
					Get: func(context.Context, ctrlcli.WithWatch, ctrlcli.ObjectKey, ctrlcli.Object, ...ctrlcli.GetOption) error {
						return errors.New("API unavailable")
					},
				})
			}
			webhook := &ModelStoreBindingWebhook{APIReader: reader}
			if tc.noReader {
				webhook.APIReader = nil
			}
			errs := webhook.validateAuthorityPin(context.Background(), old, current)
			require.Len(t, errs, 1)
			require.Equal(t, tc.want, errs[0].Type)
		})
	}
}
