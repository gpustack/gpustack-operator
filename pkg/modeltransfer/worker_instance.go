package modeltransfer

import (
	"context"
	"errors"
	"regexp"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	workerInstanceAnnotation = "model-transfer.gpustack.ai/worker-instance"
	drainForAnnotation       = "model-transfer.gpustack.ai/drain-for"
)

var workerInstancePattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// recordWorkerInstance CAS-publishes the supervisor boot on the exact live
// Pod. A requested drain permits only the already-recorded holder to reattach.
func (c *Config) recordWorkerInstance(ctx context.Context, workerID, instance string, starting bool) error {
	if workerID != c.WorkerID || !workerInstancePattern.MatchString(instance) || !c.Broker.CanBindWorkerInstance(instance) {
		return errSourceIneligible
	}
	for attempt := 0; attempt < 6; attempt++ {
		pod, err := c.localSourcePod(ctx)
		if err != nil {
			return err
		}
		current := pod.Annotations[workerInstanceAnnotation]
		draining := pod.Annotations[drainForAnnotation] != ""
		if draining && (starting || current != instance) {
			return errSourceIneligible
		}
		if !draining {
			if err := c.sourcePod(ctx); err != nil {
				return err
			}
		}
		if current == instance {
			return nil
		}
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		pod.Annotations[workerInstanceAnnotation] = instance
		if _, err := c.Client.CoreV1().Pods(c.WorkerNamespace).Update(ctx, pod, meta.UpdateOptions{}); apierrors.IsConflict(err) {
			continue
		} else if err != nil {
			return err
		}
		return nil
	}
	return errors.New("worker instance update stayed contested")
}
