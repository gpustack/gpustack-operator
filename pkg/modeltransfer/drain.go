package modeltransfer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	"gpustack.ai/gpustack/pkg/modeltransfer/broker"
)

const (
	drainedForAnnotation      = "model-transfer.gpustack.ai/drained-for"
	drainedInstanceAnnotation = "model-transfer.gpustack.ai/drained-instance"
)

func (c *Config) reconcileSource(ctx context.Context) error {
	src, ok := c.Intents.(*kubernetesIntentSource)
	if !ok {
		c.invalidateLiveIntentCache()
		c.Broker.SetAssignmentsFrozen(true)
		return errors.New("the Kubernetes intent source is unavailable")
	}
	if err := broker.ReconcileIntents(ctx, c.Broker, src); err != nil {
		c.invalidateLiveIntentCache()
		c.Broker.SetAssignmentsFrozen(true)
		// A failed pass always freezes assignments and drops the live
		// grant cache, but draining every serving generation waits for
		// sustained failure or a proved authority outage: one dropped
		// API request must not empty the node.
		c.reconcileFailures++
		if c.reconcileFailures >= reconcileFailureLimit ||
			errors.Is(err, broker.ErrReservationUnavailable) {
			if holdErr := c.Broker.InvalidateServing(); holdErr != nil {
				return fmt.Errorf("invalidate source readiness: %w", holdErr)
			}
		}
		return err
	}
	c.reconcileFailures = 0
	if src.eligible {
		c.Broker.SetAssignmentsFrozen(false)
		return nil
	}
	c.invalidateLiveIntentCache()
	c.Broker.SetAssignmentsFrozen(true)
	return c.acknowledgeDrain(ctx, src)
}

func (c *Config) acknowledgeDrain(ctx context.Context, src *kubernetesIntentSource) error {
	pod, err := c.localSourcePod(ctx)
	if err != nil {
		return err
	}
	request := pod.Annotations[drainForAnnotation]
	instance := pod.Annotations[workerInstanceAnnotation]
	if request == "" {
		return nil
	}
	if instance != "" && !workerInstancePattern.MatchString(instance) {
		return errors.New("drain awaits a recorded native worker instance")
	}
	if err := c.Broker.VerifyAuthorityInventory(ctx, src.client, c.Client.CoreV1().ConfigMaps(c.WorkerNamespace), c.WorkerNamespace); err != nil {
		return err
	}
	if !c.Broker.ReadyForDrain() {
		return errors.New("drain awaits local or global release")
	}
	frozenInstance, err := c.freezeRuntime(ctx)
	if err != nil {
		return err
	}
	if instance != "" && instance != frozenInstance {
		return errors.New("native worker instance changed during freeze")
	}
	if instance == "" {
		instance = frozenInstance
	}
	observed, err := c.inspectEmptyRuntime(ctx, instance)
	if err != nil {
		return err
	}
	if pod.Annotations[workerInstanceAnnotation] == "" {
		instance = observed
		pinned := false
		for attempt := 0; attempt < 6; attempt++ {
			pod, err := c.localSourcePod(ctx)
			if err != nil {
				return err
			}
			if pod.Annotations[drainForAnnotation] != request {
				return errors.New("drain request changed")
			}
			current := pod.Annotations[workerInstanceAnnotation]
			if current != "" && current != instance {
				return errors.New("native worker instance changed")
			}
			if current == instance {
				pinned = true
				break
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
			pinned = true
			break
		}
		if !pinned {
			return errors.New("native worker instance update stayed contested")
		}
		if err := c.observeEmptyRuntime(ctx, instance); err != nil {
			return err
		}
	}
	for attempt := 0; attempt < 6; attempt++ {
		pod, err := c.localSourcePod(ctx)
		if err != nil {
			return err
		}
		if pod.Annotations[drainForAnnotation] != request || pod.Annotations[workerInstanceAnnotation] != instance || !c.Broker.ReadyForDrain() {
			return errors.New("drain identity changed before acknowledgement")
		}
		if pod.Annotations[drainedForAnnotation] == request && pod.Annotations[drainedInstanceAnnotation] == instance {
			return nil
		}
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		pod.Annotations[drainedForAnnotation] = request
		pod.Annotations[drainedInstanceAnnotation] = instance
		if _, err := c.Client.CoreV1().Pods(c.WorkerNamespace).Update(ctx, pod, meta.UpdateOptions{}); apierrors.IsConflict(err) {
			continue
		} else if err != nil {
			return err
		}
		return nil
	}
	return errors.New("drain acknowledgement stayed contested")
}

func (c *Config) freezeRuntime(ctx context.Context) (string, error) {
	if c.runtimeURL == "" || c.bootstrapToken == "" {
		return "", errors.New("runtime freeze is unavailable")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(c.runtimeURL, "/v1/runtime")+"/v1/runtime/drain", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.bootstrapToken)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return "", errors.New("runtime freeze is unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", errors.New("runtime freeze was refused")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 || uniqueTopLevelJSON(raw) != nil {
		return "", errors.New("runtime freeze response is invalid")
	}
	var result struct {
		WorkerID         string `json:"workerID"`
		WorkerInstanceID string `json:"workerInstanceID"`
		Draining         *bool  `json:"draining"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil || result.WorkerID != c.WorkerID ||
		!workerInstancePattern.MatchString(result.WorkerInstanceID) || result.Draining == nil || !*result.Draining {
		return "", errors.New("runtime freeze identity is invalid")
	}
	return result.WorkerInstanceID, nil
}

func (c *Config) observeEmptyRuntime(ctx context.Context, instance string) error {
	_, err := c.inspectEmptyRuntime(ctx, instance)
	return err
}

func (c *Config) inspectEmptyRuntime(ctx context.Context, instance string) (string, error) {
	if c.runtimeURL == "" || c.bootstrapToken == "" {
		return "", errors.New("runtime attestation is unavailable")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.runtimeURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.bootstrapToken)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", errors.New("runtime attestation is unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", errors.New("runtime attestation was refused")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return "", errors.New("runtime attestation is invalid")
	}
	if err := uniqueTopLevelJSON(raw); err != nil {
		return "", errors.New("runtime attestation is invalid")
	}
	var result struct {
		WorkerID            string    `json:"workerID"`
		WorkerInstanceID    string    `json:"workerInstanceID"`
		Generations         *[]string `json:"generations"`
		NativeOperations    *int64    `json:"nativeOperations"`
		UnknownOperations   *int64    `json:"unknownOperations"`
		RetainedGenerations *[]string `json:"retainedGenerations"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return "", errors.New("runtime attestation is invalid")
	}
	if result.WorkerID != c.WorkerID || !workerInstancePattern.MatchString(result.WorkerInstanceID) ||
		(instance != "" && result.WorkerInstanceID != instance) || result.Generations == nil || result.RetainedGenerations == nil ||
		result.NativeOperations == nil || result.UnknownOperations == nil || len(*result.Generations) != 0 ||
		len(*result.RetainedGenerations) != 0 || *result.NativeOperations != 0 || *result.UnknownOperations != 0 {
		return "", errors.New("runtime still holds native state or has a different identity")
	}
	return result.WorkerInstanceID, nil
}

func uniqueTopLevelJSON(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return fmt.Errorf("expected an object")
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok || seen[key] {
			return errors.New("duplicate or invalid field")
		}
		seen[key] = true
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}
