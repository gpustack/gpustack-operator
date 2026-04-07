package server

import (
	"context"
	"time"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	server "gpustack.ai/gpustack/api/server/v1"
	servercore "gpustack.ai/gpustack/api/server/v1alpha1"
	"gpustack.ai/gpustack/pkg/controller"
	"gpustack.ai/gpustack/pkg/kubeappyaml"
	"gpustack.ai/gpustack/pkg/kubeclientset"
	"gpustack.ai/gpustack/pkg/server/apistatus"
	"gpustack.ai/gpustack/pkg/system"
	"gpustack.ai/gpustack/pkg/systemmeta"
)

// ClusterReconciler reconciles a v1alpha1.Cluster object.
type ClusterReconciler struct {
	Client ctrlcli.Client
}

var _ ctrlreconcile.Reconciler = (*ClusterReconciler)(nil)

func (r *ClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	// Fetch.
	cls := new(servercore.Cluster)
	err := r.Client.Get(ctx, req.NamespacedName, cls)
	if err != nil {
		logger.Error(err, "fetch cluster")
		return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
	}

	// Clean up if deleted.
	if cls.DeletionTimestamp != nil {
		// Return if already unlocked.
		if systemmeta.Unlock(cls) {
			return ctrl.Result{}, nil
		}

		// Notify deletion.
		{
			apistatus.ClusterConditionDeleting.True(cls, "", "")
			apistatus.SummarizeCluster(&cls.Status)

			err = r.Client.Status().Update(ctx, cls)
			if err != nil {
				logger.Error(err, "update cluster status when deleting")
				return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
			}
		}

		// Delete related secrets if exists.
		if cls.Status.ConfigSecretName != "" {
			sec := &core.Secret{
				ObjectMeta: meta.ObjectMeta{
					Namespace: cls.Namespace,
					Name:      cls.Status.ConfigSecretName,
				},
			}
			err = kubeclientset.DeleteWithCtrlClient(ctx, r.Client, sec)
			if err != nil {
				logger.Error(err, "delete cluster config secret when deleting cluster")
				return ctrl.Result{}, err
			}
		}

		// TODO: Eject reverse proxy if exists.

		// Unlock.
		_, err = kubeclientset.UpdateWithCtrlClient(ctx, r.Client, cls)
		if err != nil {
			logger.Error(err, "unlock cluster when deleting")
			return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
		}

		return ctrl.Result{}, nil
	}

	// Lock if not.
	if !systemmeta.Lock(cls) {
		cls, err = kubeclientset.UpdateWithCtrlClient(ctx, r.Client, cls)
		if err != nil {
			logger.Error(err, "lock cluster")
			return ctrl.Result{}, err
		}
	}

	// Nothing to do if failed to import,
	// the user can set this condition to unknown to trigger importing again.
	if apistatus.ClusterConditionImported.IsFalse(cls) {
		return ctrl.Result{}, nil
	}

	peer := system.Peer.Get()

	// If not imported, wait for importing cluster.
	if !apistatus.ClusterConditionImported.IsTrue(cls) || cls.Status.ConfigSecretName == "" {
		// For non-loopback cluster:
		// - Set to wait for importing at first.
		// - For reverse proxy cluster, it requires user to execute cluster importing,
		//   return directly if not executed.
		// - For proxy cluster, it requires user to provide config secret,
		//   return directly if not provided.
		// - For proxy cluster, if the config secret is gone after importing, clean up status and retrigger importing.
		if cls.Spec.Type != servercore.ClusterTypeLoopback {
			if !apistatus.ClusterConditionImported.Exists(cls) {
				var msg string
				switch cls.Spec.Type {
				case servercore.ClusterTypeProxy:
					msg = "Waiting for providing cluster config."
				case servercore.ClusterTypeReverseProxy:
					msg = "Waiting for executing cluster importing script."
				}
				apistatus.ClusterConditionImported.Unknown(cls, apistatus.ClusterConditionImportedReasonWaitingForImport, msg)
				apistatus.SummarizeCluster(&cls.Status)

				err = r.Client.Status().Update(ctx, cls)
				if err != nil {
					logger.Error(err, "update cluster status when waiting for importing")
					return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
				}
				return ctrl.Result{}, nil
			}

			if cls.Spec.Type == servercore.ClusterTypeReverseProxy {
				return ctrl.Result{}, nil
			}

			if cls.Status.ConfigSecretName == "" {
				if apistatus.ClusterConditionImported.IsUnknown(cls) {
					return ctrl.Result{}, nil
				}

				// Clean up status and retrigger.
				cls.Status = servercore.ClusterStatus{}

				err = r.Client.Status().Update(ctx, cls)
				if err != nil {
					logger.Error(err, "update cluster status when waiting for importing")
				}
				return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
			}
		}

		// Otherwise, apply importing config actively.
		if !apistatus.ClusterConditionImported.IsTrue(cls) {
			clsRestCfg, err := peer.GetClusterKubeRestConfig(ctx, ctrlcli.ObjectKeyFromObject(cls))
			if err != nil {
				logger.Errorf(err, "get cluster kube rest config when applying import config")
				return ctrl.Result{}, err
			}

			clsImpCfg := new(server.ClusterImportConfig)
			err = r.Client.SubResource("importconfig").Get(ctx, (*server.Cluster)(cls), clsImpCfg)
			if err != nil {
				logger.Errorf(err, "get cluster import config when applying import config")
				return ctrl.Result{}, err
			}

			err = kubeappyaml.Apply(ctx, clsImpCfg.Status.Config, *clsRestCfg)
			if err != nil {
				logger.Error(err, "apply cluster import config")
				apistatus.ClusterConditionImported.False(cls, apistatus.ClusterConditionImportedReasonApplyingConfig, "Failed to apply cluster import config")
				apistatus.SummarizeCluster(&cls.Status)

				err = r.Client.Status().Update(ctx, cls)
				if err != nil {
					logger.Error(err, "update cluster status when applying import config")
				}
				return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
			}
			apistatus.ClusterConditionImported.True(cls, "", "")
		}
	}

	// Check connectivity and update status.
	clsMetadata, err := peer.GetClusterMetadata(ctx, ctrlcli.ObjectKeyFromObject(cls))
	if err != nil {
		apistatus.ClusterConditionConnected.False(cls, apistatus.ClusterConditionConnectedReasonDisconnected, err.Error())
		apistatus.SummarizeCluster(&cls.Status)
	} else {
		apistatus.ClusterConditionConnected.True(cls, "", "")
		apistatus.SummarizeCluster(&cls.Status)
		cls.Status.Endpoint = clsMetadata.Endpoint
		cls.Status.Version = clsMetadata.Version
		cls.Status.CA = clsMetadata.CA
	}

	err = r.Client.Status().Update(ctx, cls)
	if err != nil {
		logger.Error(err, "update cluster status")
		return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
	}

	return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
}

func (r *ClusterReconciler) SetupController(ctx context.Context, opts controller.SetupOptions) error {
	r.Client = opts.Manager.GetClient()

	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("server.manage.clusters").
		For(
			// Focus on the Cluster.
			&servercore.Cluster{},
		).
		Complete(r)
}
