package modeltransfer

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ktesting "k8s.io/client-go/testing"

	kubefake "gpustack.ai/gpustack/pkg/kubeclients/kubernetes/fake"
	"gpustack.ai/gpustack/pkg/modelmanager/store"
	"gpustack.ai/gpustack/pkg/modeltransfer/broker"
)

type startupIntents struct{}

func (startupIntents) ListIntents(context.Context) ([]broker.Intent, error) { return nil, nil }

func TestStartupPodObservationsCannotProveNativeRelease(t *testing.T) {
	for _, tc := range []struct {
		name        string
		phase       core.PodPhase
		absent      bool
		terminating bool
		apiError    bool
		current     bool
	}{
		{name: "live", phase: core.PodRunning},
		{name: "current", phase: core.PodRunning, current: true},
		{name: "terminating", phase: core.PodRunning, terminating: true},
		{name: "failed", phase: core.PodFailed},
		{name: "succeeded", phase: core.PodSucceeded},
		{name: "absent", absent: true},
		{name: "API unavailable", apiError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			st, err := store.Open(filepath.Join(root, "cache"))
			require.NoError(t, err)
			limits := broker.Limits{PhysicalBytes: 1024, MaxGenerations: 4, MaxQuarantined: 4, QuarantineBytes: 1024}
			ledger := filepath.Join(root, "broker")
			initial, err := broker.Open(ledger, "nvidia", st, limits, time.Now)
			require.NoError(t, err)
			require.NoError(t, initial.Close())
			record := broker.GenerationRecord{
				Generation: "1", Digest: "sha256:" + strings.Repeat("a", 64), Namespace: "tenant",
				WorkerID: "old-source", State: broker.StateServing, NativeStarted: true,
				NativeInstance: "old-instance", NativeStartEnforced: true,
				ReservedBytes: 13, ResidentBytes: 12, ActiveSessions: 2, UnknownSessions: 2,
			}
			raw, err := json.Marshal(record)
			require.NoError(t, err)
			recordFile := filepath.Join(ledger, "generations", "1.json")
			require.NoError(t, os.WriteFile(recordFile, raw, 0o600))
			client := kubefake.NewSimpleClientset()
			if !tc.absent {
				pod := &core.Pod{
					ObjectMeta: meta.ObjectMeta{Name: "source", Namespace: "operator", UID: types.UID(record.WorkerID)},
					Spec:       core.PodSpec{NodeName: "node"}, Status: core.PodStatus{Phase: tc.phase},
				}
				if tc.terminating {
					now := meta.Now()
					pod.DeletionTimestamp = &now
				}
				_, err = client.CoreV1().Pods("operator").Create(context.Background(), pod, meta.CreateOptions{})
				require.NoError(t, err)
			}
			if tc.apiError {
				client.PrependReactor("list", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
					return true, nil, errors.New("API unavailable")
				})
			}
			for range 2 {
				b, openErr := broker.Open(ledger, "nvidia", st, limits, time.Now)
				require.NoError(t, openErr)
				cfg := &Config{
					Broker: b, Client: client, Intents: startupIntents{}, WorkerID: "replacement",
					NodeName: "node", WorkerNamespace: "operator", WorkerLabel: labels.Everything(),
					ControlAddr: "missing-port",
				}
				if tc.current {
					cfg.WorkerID = record.WorkerID
				}
				require.ErrorContains(t, cfg.Run(context.Background()), "listen on the control address")
				require.False(t, b.CanBindWorkerInstance("replacement-instance"))
				raw, err = os.ReadFile(recordFile)
				require.NoError(t, err)
				var observed broker.GenerationRecord
				require.NoError(t, json.Unmarshal(raw, &observed))
				require.False(t, observed.NativeReleased)
				require.Equal(t, record.ReservedBytes, observed.ReservedBytes)
				require.Equal(t, record.ResidentBytes, observed.ResidentBytes)
				require.Equal(t, record.UnknownSessions, observed.UnknownSessions)
				require.Equal(t, record.ActiveSessions, observed.ActiveSessions)
				require.Contains(t, []string{broker.StateQuarantined, broker.StateDraining}, observed.State)
				require.NoError(t, b.Close())
			}
		})
	}
}
