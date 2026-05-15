package kuberess

import (
	"context"
	"fmt"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	klog "k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	server "gpustack.ai/gpustack/api/server/v1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes"
	"gpustack.ai/gpustack/pkg/kubeclientset"
	"gpustack.ai/gpustack/pkg/server/settings"
	"gpustack.ai/gpustack/pkg/utils/osx"
	"gpustack.ai/gpustack/pkg/utils/stringx"
)

// AdminSubjectName is the name of the admin subject.
const AdminSubjectName = "admin"

// InstallAdminSubject creates the admin subject,
// alias to Kubernetes Secret gpustack-subject-admin under the system namespace.
func InstallAdminSubject(ctx context.Context, cli kubernetes.Interface, password string) error {
	subCli := cli.ServerV1().Subjects(SystemNamespaceName)
	_, err := subCli.Get(ctx, AdminSubjectName,
		meta.GetOptions{
			ResourceVersion: "0",
		})
	if err != nil && kerrors.IsNotFound(err) && password == "" {
		// NB(thxCode): in order to avoid multiple GPUStack get different bootstrap password,
		// we will save the bootstrap password to the Kubernetes Secret gpustack-subject-admin-bootstrap-password.
		randomPwd := stringx.RandomString(16)
		secCli := cli.CoreV1().Secrets(SystemNamespaceName)
		sec := &core.Secret{
			ObjectMeta: meta.ObjectMeta{
				Namespace: SystemNamespaceName,
				Name:      "gpustack-subject-admin-bootstrap-password",
			},
			StringData: map[string]string{
				"password": randomPwd,
			},
		}
		sec, err := kubeclientset.Create(ctx, secCli, sec)
		if err != nil {
			return fmt.Errorf("create random bootstrap password secret: %w", err)
		}

		// Update the bootstrap password provision if the random password has been accepted.
		if randomPwd == string(sec.Data["password"]) {
			provision := "process"
			switch {
			case osx.ExistEnv("KUBERNETES_SERVICE_HOST"):
				provision = "kubernetes"
			case osx.ExistEnv("_RUNNING_INSIDE_CONTAINER_"):
				provision = "docker"
			}
			_ = settings.BootstrapPasswordProvisionState.Configure(ctx, provision)
		}

		password = string(sec.Data["password"])

		// Print out.
		klog.Infof("!!! bootstrap admin password: %s !!!", password)
	}

	// Return if the admin subject already exists.
	if err == nil {
		return nil
	}

	// Create subject.
	subj := &server.Subject{
		ObjectMeta: meta.ObjectMeta{
			Namespace: SystemNamespaceName,
			Name:      AdminSubjectName,
		},
		Spec: server.SubjectSpec{
			Provider:    DefaultSubjectProviderName,
			Role:        server.SubjectRoleAdmin,
			DisplayName: "Administrator",
			Description: "The administrator subject created by GPUStack.",
			Email:       "contact@gpustack.ai",
			Credential:  ptr.To(password),
		},
	}

	_, err = kubeclientset.Create(ctx, subCli, subj)
	if err != nil {
		return fmt.Errorf("install %s subject: %w", AdminSubjectName, err)
	}

	return nil
}
