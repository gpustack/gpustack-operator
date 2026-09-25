package worker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	authn "k8s.io/api/authentication/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clienttesting "k8s.io/client-go/testing"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	kubefake "gpustack.ai/gpustack/pkg/kubeclients/kubernetes/fake"
	"gpustack.ai/gpustack/pkg/webhook"
)

const testWorkerUsername = "system:serviceaccount:gpustack-system:gpustack-operator-worker"

// admissionContext is ctx carrying an UPDATE request from username, of subresource sub.
func admissionContext(username, sub string) context.Context {
	return ctrladmission.NewContextWithRequest(context.Background(), ctrladmission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation:   admissionv1.Update,
			SubResource: sub,
			UserInfo:    authn.UserInfo{Username: username},
		},
	})
}

func TestModelArtifactStatusGuard(t *testing.T) {
	cases := []struct {
		name        string
		worker      string
		username    string
		sub         string
		terminating bool
		edit        func(*workercore.ModelArtifact)
		wantRule    string
	}{
		{name: "the worker writes status", worker: testWorkerUsername, username: testWorkerUsername, sub: "status"},
		{
			name: "a tenant writes status", worker: testWorkerUsername, sub: "status",
			username: "system:serviceaccount:team-a:default", wantRule: modelArtifactStatusRuleWriter,
		},
		{
			name: "an administrator writes status", worker: testWorkerUsername, sub: "status",
			username: "kubernetes-admin", wantRule: modelArtifactStatusRuleWriter,
		},
		{
			name: "a tenant writes the status of a Terminating artifact", worker: testWorkerUsername, sub: "status",
			username: "system:serviceaccount:team-a:default", terminating: true, wantRule: modelArtifactStatusRuleWriter,
		},
		{
			name: "no worker identity learned refuses every writer", sub: "status",
			username: testWorkerUsername, wantRule: modelArtifactStatusRuleWriter,
		},
		{
			name: "a tenant's metadata update of the main resource keeps its own rules", worker: testWorkerUsername,
			username: "system:serviceaccount:team-a:default",
			edit:     func(ma *workercore.ModelArtifact) { ma.Labels = map[string]string{"a": "b"} },
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			old := newTestHubArtifact("owner/repo", "main")
			if c.terminating {
				now := meta.Now()
				old.DeletionTimestamp = &now
				old.Finalizers = []string{"worker.gpustack.ai/model-artifact-protection"}
			}
			updated := old.DeepCopy()
			updated.Status.Resolved = &workercore.ModelArtifactResolved{
				Revision: strings.Repeat("a", 40), ManifestDigest: "sha256:" + strings.Repeat("f", 64),
			}
			if c.edit != nil {
				c.edit(updated)
			}

			_, err := (&ModelArtifactWebhook{worker: c.worker}).ValidateUpdate(admissionContext(c.username, c.sub), old, updated)
			if c.wantRule == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.wantRule)
			assert.Contains(t, err.Error(), c.username)
		})
	}
}

// TestModelArtifactStatusGuardHoldsDuringDeletion pins the opt-out the Terminating case relies on:
// without it the framework skips ValidateUpdate for an object carrying a deletion timestamp, and a
// tenant could write the status of a Terminating artifact, which stays mountable while referenced.
func TestModelArtifactStatusGuardHoldsDuringDeletion(t *testing.T) {
	_, ok := any(new(ModelArtifactWebhook)).(webhook.ReceiveDeletionUpdate)
	assert.True(t, ok)
}

// TestLearnUsername pins where the status rule's writer comes from: SelfSubjectReview where the API
// server serves it, and a TokenReview of the worker's own token where it does not (before 1.28).
func TestLearnUsername(t *testing.T) {
	notServed := kerrors.NewNotFound(schema.GroupResource{Group: "authentication.k8s.io", Resource: "selfsubjectreviews"}, "")
	cases := []struct {
		name string
		// ssrErr is what SelfSubjectReview answers; nil answers testWorkerUsername.
		ssrErr error
		token  func() (string, error)
		// tokenUser is the user a TokenReview authenticates, "" for none.
		tokenUser     string
		wantUser      string
		wantErr       string
		wantTokenSent bool
	}{
		{name: "SelfSubjectReview answers where it is served", wantUser: testWorkerUsername},
		{
			name: "an API server without SelfSubjectReview is asked with a TokenReview", ssrErr: notServed,
			token: func() (string, error) { return "worker-token", nil }, tokenUser: testWorkerUsername,
			wantUser: testWorkerUsername, wantTokenSent: true,
		},
		{
			name: "no bearer token to review stops the worker", ssrErr: notServed,
			token:   func() (string, error) { return "", errors.New("no bearer token") },
			wantErr: "serves no SelfSubjectReview",
		},
		{
			name: "a token the API server does not authenticate stops the worker", ssrErr: notServed,
			token:   func() (string, error) { return "worker-token", nil },
			wantErr: "did not authenticate", wantTokenSent: true,
		},
		{
			name:    "another SelfSubjectReview failure is not taken for an old API server",
			ssrErr:  kerrors.NewForbidden(schema.GroupResource{Group: "authentication.k8s.io", Resource: "selfsubjectreviews"}, "", errors.New("denied")),
			token:   func() (string, error) { return "worker-token", nil },
			wantErr: "self subject review",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cli := kubefake.NewClientset()
			cli.PrependReactor("create", "selfsubjectreviews", func(clienttesting.Action) (bool, runtime.Object, error) {
				if c.ssrErr != nil {
					return true, nil, c.ssrErr
				}
				return true, &authn.SelfSubjectReview{Status: authn.SelfSubjectReviewStatus{UserInfo: authn.UserInfo{Username: testWorkerUsername}}}, nil
			})
			var sent string
			cli.PrependReactor("create", "tokenreviews", func(a clienttesting.Action) (bool, runtime.Object, error) {
				tr := a.(clienttesting.CreateAction).GetObject().(*authn.TokenReview)
				sent = tr.Spec.Token
				tr.Status = authn.TokenReviewStatus{Authenticated: c.tokenUser != "", User: authn.UserInfo{Username: c.tokenUser}}
				return true, tr, nil
			})

			got, err := learnUsername(context.Background(), cli, c.token)
			assert.Equal(t, c.wantTokenSent, sent == "worker-token")
			if c.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), c.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, c.wantUser, got)

			// The learned username is the one the status rule admits.
			wh := &ModelArtifactWebhook{worker: got}
			ma := &workercore.ModelArtifact{ObjectMeta: meta.ObjectMeta{Name: "qwen", Namespace: "team-a"}}
			assert.NoError(t, wh.validateModelArtifactStatus(admissionContext(testWorkerUsername, "status"), ma))
		})
	}
}
