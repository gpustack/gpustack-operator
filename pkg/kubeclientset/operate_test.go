package kubeclientset

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admreg "k8s.io/api/admissionregistration/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kubefake "gpustack.ai/gpustack/pkg/kubeclients/kubernetes/fake"
	"gpustack.ai/gpustack/pkg/kubemeta"
)

const (
	testVwcName      = "gpustack-worker-validation"
	testVwcResource  = "validatingwebhookconfigurations"
	expectedWebhook  = "expected.gpustack.ai"
	staleWebhook     = "stale.gpustack.ai"
	peerWebhookOnCli = "peer.gpustack.ai"
)

var testVwcGroupResource = schema.GroupResource{
	Group:    admreg.SchemeGroupVersion.Group,
	Resource: testVwcResource,
}

// newTestVwc builds a validating webhook configuration holding one webhook of the given name.
func newTestVwc(webhook string) *admreg.ValidatingWebhookConfiguration {
	return &admreg.ValidatingWebhookConfiguration{
		ObjectMeta: meta.ObjectMeta{
			Name: testVwcName,
		},
		Webhooks: []admreg.ValidatingWebhook{
			{
				Name: webhook,
			},
		},
	}
}

// alignTestVwc returns the align function shape the webhook configuration installer supplies:
// it overwrites the webhook list of the actual object, and reports skip if it already matches.
func alignTestVwc(expected *admreg.ValidatingWebhookConfiguration) AlignWithFn[*admreg.ValidatingWebhookConfiguration] {
	return func(actual *admreg.ValidatingWebhookConfiguration) (*admreg.ValidatingWebhookConfiguration, bool, error) {
		if kubemeta.DeepEqual(actual.Webhooks, expected.Webhooks) {
			return actual, true, nil
		}
		actual.Webhooks = expected.DeepCopy().Webhooks
		return actual, false, nil
	}
}

// getTestVwc reads the validating webhook configuration back from the cluster.
func getTestVwc(t *testing.T, cli *kubefake.Clientset) *admreg.ValidatingWebhookConfiguration {
	t.Helper()
	actual, err := cli.AdmissionregistrationV1().ValidatingWebhookConfigurations().
		Get(t.Context(), testVwcName, meta.GetOptions{})
	require.NoError(t, err)
	require.Len(t, actual.Webhooks, 1)
	return actual
}

// flakyUpdates rejects the first n update calls with a conflict error, which is the only way
// to drive the losing writer: the fake object tracker implements no optimistic concurrency,
// so a conflict never arises on its own.
type flakyUpdates struct {
	mu        sync.Mutex
	remain    int
	calls     int
	conflicts int
}

func (f *flakyUpdates) react(k8stesting.Action) (bool, runtime.Object, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls++
	if f.remain > 0 {
		f.remain--
		f.conflicts++
		return true, nil, kerrors.NewConflict(testVwcGroupResource, testVwcName,
			errors.New("the object has been modified"))
	}

	return false, nil, nil
}

func (f *flakyUpdates) stats() (calls, conflicts int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.conflicts
}

// losingCreate rejects the first create call with an already-existed error, after seeding the
// object a peer writer created, which is how a replica that loses the create observes the world.
type losingCreate struct {
	mu      sync.Mutex
	tracker k8stesting.ObjectTracker
	peer    *admreg.ValidatingWebhookConfiguration
	done    bool
}

func (l *losingCreate) react(k8stesting.Action) (bool, runtime.Object, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.done {
		return false, nil, nil
	}
	l.done = true

	if err := l.tracker.Add(l.peer); err != nil {
		return true, nil, err
	}

	return true, nil, kerrors.NewAlreadyExists(testVwcGroupResource, testVwcName)
}

// Test_Update_conflict covers the contract the CRD and webhook configuration installers rely
// on: a conflicting update retries when an align function is supplied, and is returned to the
// caller when it is not.
func Test_Update_conflict(t *testing.T) {
	expected := newTestVwc(expectedWebhook)

	testCases := []struct {
		name            string
		seed            *admreg.ValidatingWebhookConfiguration
		conflicts       int
		align           bool
		wantConflictErr bool
		wantUpdateCalls int
		wantWebhook     string
	}{
		{
			name:            "align function retries a conflict",
			seed:            newTestVwc(staleWebhook),
			conflicts:       1,
			align:           true,
			wantUpdateCalls: 2,
			wantWebhook:     expectedWebhook,
		},
		{
			name:            "no align function returns the conflict",
			seed:            newTestVwc(staleWebhook),
			conflicts:       1,
			wantConflictErr: true,
			wantUpdateCalls: 1,
			wantWebhook:     staleWebhook,
		},
		{
			name:            "align function skips an aligned object",
			seed:            newTestVwc(expectedWebhook),
			align:           true,
			wantUpdateCalls: 0,
			wantWebhook:     expectedWebhook,
		},
		{
			name:            "creates an absent object",
			align:           true,
			wantUpdateCalls: 0,
			wantWebhook:     expectedWebhook,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var objs []runtime.Object
			if tc.seed != nil {
				objs = append(objs, tc.seed)
			}
			cli := kubefake.NewSimpleClientset(objs...)

			updates := &flakyUpdates{remain: tc.conflicts}
			cli.PrependReactor("update", testVwcResource, updates.react)

			opts := []UpdateOption[*admreg.ValidatingWebhookConfiguration]{
				WithCreateIfNotExisted[*admreg.ValidatingWebhookConfiguration](),
			}
			if tc.align {
				opts = append(opts, WithUpdateAlign(alignTestVwc(expected)))
			}

			_, err := Update(t.Context(),
				cli.AdmissionregistrationV1().ValidatingWebhookConfigurations(), expected, opts...)
			if tc.wantConflictErr {
				require.Error(t, err)
				assert.True(t, kerrors.IsConflict(err), "want a conflict error, got %v", err)
			} else {
				require.NoError(t, err)
			}

			calls, _ := updates.stats()
			assert.Equal(t, tc.wantUpdateCalls, calls, "update calls")
			assert.Equal(t, tc.wantWebhook, getTestVwc(t, cli).Webhooks[0].Name)
		})
	}
}

// Test_Create_conflict covers the contract the extension API service installer relies on: with
// an align function supplied, a writer that loses the create and a writer that loses the update
// both converge on the expected object.
func Test_Create_conflict(t *testing.T) {
	expected := newTestVwc(expectedWebhook)

	testCases := []struct {
		name            string
		seed            *admreg.ValidatingWebhookConfiguration
		peer            *admreg.ValidatingWebhookConfiguration
		conflicts       int
		wantUpdateCalls int
	}{
		{
			name:            "align function retries a conflict",
			seed:            newTestVwc(staleWebhook),
			conflicts:       1,
			wantUpdateCalls: 2,
		},
		{
			name:            "align function skips an aligned object",
			seed:            newTestVwc(expectedWebhook),
			wantUpdateCalls: 0,
		},
		{
			name:            "creates an absent object",
			wantUpdateCalls: 0,
		},
		{
			name:            "a peer winning the create converges through update",
			peer:            newTestVwc(peerWebhookOnCli),
			wantUpdateCalls: 1,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var objs []runtime.Object
			if tc.seed != nil {
				objs = append(objs, tc.seed)
			}
			cli := kubefake.NewSimpleClientset(objs...)

			updates := &flakyUpdates{remain: tc.conflicts}
			cli.PrependReactor("update", testVwcResource, updates.react)
			if tc.peer != nil {
				creates := &losingCreate{tracker: cli.Tracker(), peer: tc.peer}
				cli.PrependReactor("create", testVwcResource, creates.react)
			}

			_, err := Create(t.Context(),
				cli.AdmissionregistrationV1().ValidatingWebhookConfigurations(), expected,
				WithUpdateIfExisted(alignTestVwc(expected)))
			require.NoError(t, err)

			calls, _ := updates.stats()
			assert.Equal(t, tc.wantUpdateCalls, calls, "update calls")
			assert.Equal(t, expectedWebhook, getTestVwc(t, cli).Webhooks[0].Name)
		})
	}
}

// Test_CreateWithCtrlClient_conflict is Test_Create_conflict for the controller-runtime client,
// whose retry has the same shape.
func Test_CreateWithCtrlClient_conflict(t *testing.T) {
	expected := newTestVwc(expectedWebhook)

	scheme := runtime.NewScheme()
	require.NoError(t, admreg.AddToScheme(scheme))

	var conflicts int
	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(newTestVwc(staleWebhook)).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(
				ctx context.Context,
				c ctrlcli.WithWatch,
				obj ctrlcli.Object,
				opts ...ctrlcli.UpdateOption,
			) error {
				if conflicts == 0 {
					conflicts++
					return kerrors.NewConflict(testVwcGroupResource, testVwcName,
						errors.New("the object has been modified"))
				}
				return c.Update(ctx, obj, opts...)
			},
		}).
		Build()

	_, err := CreateWithCtrlClient(t.Context(), cli, expected,
		WithUpdateIfExisted(alignTestVwc(expected)))
	require.NoError(t, err)
	assert.Equal(t, 1, conflicts, "the conflict was not exercised")

	actual := &admreg.ValidatingWebhookConfiguration{}
	require.NoError(t, cli.Get(t.Context(), ctrlcli.ObjectKey{Name: testVwcName}, actual))
	require.Len(t, actual.Webhooks, 1)
	assert.Equal(t, expectedWebhook, actual.Webhooks[0].Name)
}

// Test_concurrentWritersConverge drives the concurrent-boot path: several writers apply the same
// object at once and the first of them loses an update to a conflict. Every writer must converge
// on the expected object, because a replica that fails here crashes before leader election.
func Test_concurrentWritersConverge(t *testing.T) {
	const writers = 8

	expected := newTestVwc(expectedWebhook)
	vwcCli := func(cli *kubefake.Clientset) UpdateClient[*admreg.ValidatingWebhookConfiguration] {
		return cli.AdmissionregistrationV1().ValidatingWebhookConfigurations()
	}

	testCases := []struct {
		name  string
		write func(ctx context.Context, cli *kubefake.Clientset) error
	}{
		{
			name: "Update",
			write: func(ctx context.Context, cli *kubefake.Clientset) error {
				_, err := Update(ctx, vwcCli(cli), expected,
					WithCreateIfNotExisted[*admreg.ValidatingWebhookConfiguration](),
					WithUpdateAlign(alignTestVwc(expected)))
				return err
			},
		},
		{
			name: "Create",
			write: func(ctx context.Context, cli *kubefake.Clientset) error {
				_, err := Create(ctx, cli.AdmissionregistrationV1().ValidatingWebhookConfigurations(), expected,
					WithUpdateIfExisted(alignTestVwc(expected)))
				return err
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cli := kubefake.NewSimpleClientset(newTestVwc(staleWebhook))

			updates := &flakyUpdates{remain: writers}
			cli.PrependReactor("update", testVwcResource, updates.react)

			var (
				wg   sync.WaitGroup
				errs = make([]error, writers)
			)
			for i := range writers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					errs[i] = tc.write(t.Context(), cli)
				}()
			}
			wg.Wait()

			for i := range errs {
				require.NoError(t, errs[i], "writer %d", i)
			}

			_, conflicts := updates.stats()
			assert.Positive(t, conflicts, "no conflict was exercised")
			assert.Equal(t, expectedWebhook, getTestVwc(t, cli).Webhooks[0].Name)
		})
	}
}
