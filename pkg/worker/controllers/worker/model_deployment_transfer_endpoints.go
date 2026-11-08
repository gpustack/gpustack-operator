package worker

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"slices"
	"sort"

	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlpredicate "sigs.k8s.io/controller-runtime/pkg/predicate"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/modelstore"
	"gpustack.ai/gpustack/pkg/setting"
	"gpustack.ai/gpustack/pkg/utils/stringx"
)

const modelTransferClientMount = "/var/run/gpustack/model-transfer"

func modelTransferDiscoveryPredicate() ctrlpredicate.Predicate {
	return ctrlpredicate.NewPredicateFuncs(func(o ctrlcli.Object) bool {
		return o.GetNamespace() == setting.DelegatedSecretNamespace &&
			o.GetLabels()["app.kubernetes.io/component"] == "model-transfer"
	})
}

// modelTransferSourceDaemonSetChanged ignores status-only controller updates.
func modelTransferSourceDaemonSetChanged() ctrlpredicate.Predicate {
	selected := modelTransferDiscoveryPredicate()
	return ctrlpredicate.Funcs{
		CreateFunc:  selected.Create,
		DeleteFunc:  selected.Delete,
		GenericFunc: selected.Generic,
		UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
			old, oldOK := e.ObjectOld.(*apps.DaemonSet)
			next, nextOK := e.ObjectNew.(*apps.DaemonSet)
			if !oldOK || !nextOK || (!selected.Create(ctrlevent.CreateEvent{Object: old}) && !selected.Update(e)) {
				return false
			}
			return old.UID != next.UID || !maps.Equal(old.Labels, next.Labels) ||
				!kubemeta.DeepEqual(old.DeletionTimestamp, next.DeletionTimestamp) ||
				!kubemeta.DeepEqual(old.Spec, next.Spec)
		},
	}
}

// modelTransferSourcePodChanged ignores probe and container-status churn.
func modelTransferSourcePodChanged() ctrlpredicate.Predicate {
	selected := modelTransferDiscoveryPredicate()
	return ctrlpredicate.Funcs{
		CreateFunc:  selected.Create,
		DeleteFunc:  selected.Delete,
		GenericFunc: selected.Generic,
		UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
			old, oldOK := e.ObjectOld.(*core.Pod)
			next, nextOK := e.ObjectNew.(*core.Pod)
			if !oldOK || !nextOK || (!selected.Update(e)) {
				return false
			}
			return old.UID != next.UID || old.Spec.NodeName != next.Spec.NodeName ||
				old.Status.PodIP != next.Status.PodIP || old.Status.Phase != next.Status.Phase ||
				old.Annotations["model-transfer.gpustack.ai/drain-for"] != next.Annotations["model-transfer.gpustack.ai/drain-for"] ||
				!kubemeta.DeepEqual(old.DeletionTimestamp, next.DeletionTimestamp) ||
				!maps.Equal(old.Labels, next.Labels) ||
				!kubemeta.DeepEqual(old.Spec.Containers, next.Spec.Containers) ||
				!kubemeta.DeepEqual(old.OwnerReferences, next.OwnerReferences)
		},
	}
}

// modelTransferEndpoint is public discovery information. Native keys and
// registration addresses are available only through an authenticated session.
type modelTransferEndpoint = modelstore.ModelTransferEndpoint

// reconcileModelTransferClientConfig mirrors public trust and exact Pod endpoints
// into a ConfigMap owned by the deployment. A stable mount lets later loads read
// fresh discovery without rolling replicas that already own their weights.
//
// A ModelExpress deployment reconciles it UNDER EVERY DELIVERY, not only the remote
// one: its engine reads the discovery view to find a ready source to subscribe to, and
// a seed replica without one still publishes itself into it. An empty view is the
// honest answer a seed replica can read.
func (r *ModelDeploymentReconciler) reconcileModelTransferClientConfig(ctx context.Context,
	md *workercore.ModelDeployment, weights *modelArtifactWeights,
) error {
	remote := weights != nil && weights.Render != nil &&
		weights.Render.Delivery == workercore.ModelDeploymentModelDeliveryRemote
	if !remote {
		modelExpress := modelDeploymentUsesModelExpress(md) &&
			weights != nil && weights.Render != nil &&
			weights.Render.Hub == modelDeploymentHubHuggingFace
		if !modelExpress {
			return nil
		}
	}
	services := new(core.ServiceList)
	if err := r.Client.List(ctx, services, ctrlcli.InNamespace(setting.DelegatedSecretNamespace),
		ctrlcli.MatchingLabels{"app.kubernetes.io/component": "model-transfer"}); err != nil {
		return err
	}
	pods := new(core.PodList)
	if len(services.Items) > 0 {
		if err := r.Client.List(ctx, pods, ctrlcli.InNamespace(setting.DelegatedSecretNamespace),
			ctrlcli.MatchingLabels{"app.kubernetes.io/component": "model-transfer"}); err != nil {
			return err
		}
	}
	data := map[string]string{}
	endpoints := make([]modelTransferEndpoint, 0)
	daemons := map[ctrlcli.ObjectKey]*apps.DaemonSet{}
	for _, service := range services.Items {
		vendor := service.Labels["gpustack.ai/manufacturer"]
		if vendor != "nvidia" && vendor != "ascend" {
			continue
		}
		if service.DeletionTimestamp != nil || service.Spec.ClusterIP != core.ClusterIPNone || len(service.Spec.Selector) == 0 {
			continue
		}
		port := slices.IndexFunc(service.Spec.Ports, func(p core.ServicePort) bool {
			return p.Name == "sessions" && p.Port == 9251 && (p.Protocol == "" || p.Protocol == core.ProtocolTCP)
		})
		if port < 0 {
			continue
		}
		secret := new(core.Secret)
		if err := r.Client.Get(ctx, ctrlcli.ObjectKey{Namespace: service.Namespace, Name: service.Name + "-cert"}, secret); err != nil {
			if kerrors.IsNotFound(err) {
				continue
			}
			return err
		}
		if secret.Labels["app.kubernetes.io/component"] != "model-transfer" ||
			secret.Labels["gpustack.ai/manufacturer"] != vendor {
			continue
		}
		ca := secret.Data["ca.crt"]
		if len(ca) == 0 {
			ca = secret.Data[core.TLSCertKey]
		}
		if len(ca) > 256<<10 || !x509.NewCertPool().AppendCertsFromPEM(ca) {
			ctrllog.FromContext(ctx).Info("skip source with invalid model-transfer public trust anchor", "service", ctrlcli.ObjectKeyFromObject(&service))
			continue
		}
		// Distinct releases may use different CAs for the same manufacturer.
		caName := "ca-" + stringx.SumBySHA256(service.Namespace + "/" + service.Name)[:16] + ".crt"
		data[caName] = string(ca)
		selector := labels.SelectorFromSet(service.Spec.Selector)
		for _, pod := range pods.Items {
			if pod.Namespace != service.Namespace || !selector.Matches(labels.Set(pod.Labels)) {
				continue
			}
			owner := meta.GetControllerOf(&pod)
			if pod.DeletionTimestamp != nil || pod.Status.Phase != core.PodRunning || pod.UID == "" ||
				net.ParseIP(pod.Status.PodIP) == nil || pod.Spec.NodeName == "" ||
				pod.Labels["app.kubernetes.io/component"] != "model-transfer" || pod.Labels["gpustack.ai/manufacturer"] != vendor ||
				owner == nil || owner.APIVersion != apps.SchemeGroupVersion.String() || owner.Kind != "DaemonSet" {
				continue
			}
			key := ctrlcli.ObjectKey{Namespace: pod.Namespace, Name: owner.Name}
			daemon, seen := daemons[key]
			if !seen {
				daemon = new(apps.DaemonSet)
				if err := r.Client.Get(ctx, key, daemon); err != nil {
					if !kerrors.IsNotFound(err) {
						return err
					}
					daemon = nil
				}
				daemons[key] = daemon
			}
			if daemon == nil || daemon.UID != owner.UID || daemon.DeletionTimestamp != nil ||
				daemon.Labels["app.kubernetes.io/component"] != "model-transfer" || daemon.Labels["gpustack.ai/manufacturer"] != vendor {
				continue
			}
			endpoints = append(endpoints, modelTransferEndpoint{
				Manufacturer: vendor, Address: net.JoinHostPort(pod.Status.PodIP, "9251"),
				ServerName: service.Name + "." + service.Namespace + ".svc", CAFile: modelTransferClientMount + "/" + caName,
				PodUID: string(pod.UID), PodName: pod.Name, PodNamespace: pod.Namespace, NodeName: pod.Spec.NodeName,
			})
		}
	}
	if len(endpoints) > 4096 {
		return errors.New("model-transfer source discovery exceeds its bound")
	}
	sort.Slice(endpoints, func(i, j int) bool { return endpoints[i].PodUID < endpoints[j].PodUID })
	raw, err := json.Marshal(modelstore.ModelTransferDiscovery{Version: 1, Endpoints: endpoints})
	if err != nil {
		return err
	}
	data["endpoints.json"] = string(raw)
	const maxClientConfigBytes = 900 << 10
	var totalBytes int
	for k, v := range data {
		totalBytes += len(k) + len(v)
		if totalBytes > maxClientConfigBytes {
			return errors.New("model-transfer source discovery ConfigMap exceeds its byte bound")
		}
	}
	name := "model-transfer-" + stringx.SumBySHA256(string(md.UID))[:24]
	next := &core.ConfigMap{ObjectMeta: meta.ObjectMeta{Name: name, Namespace: md.Namespace}, Data: data}
	kubemeta.ControlOn(next, md, workercore.SchemeGroupVersion.WithKind("ModelDeployment"))
	actual := new(core.ConfigMap)
	err = r.Client.Get(ctx, ctrlcli.ObjectKeyFromObject(next), actual)
	switch {
	case kerrors.IsNotFound(err):
		err = r.Client.Create(ctx, next)
	case err != nil:
		return err
	default:
		owner := meta.GetControllerOf(actual)
		if owner == nil || owner.UID != md.UID || owner.Kind != "ModelDeployment" {
			return fmt.Errorf("model-transfer client ConfigMap is owned by another object")
		}
		if !maps.Equal(actual.Data, data) {
			actual.Data = data
			err = r.Client.Update(ctx, actual)
		}
	}
	if err == nil {
		weights.Render.ModelTransferConfigMap = name
	}
	return err
}
