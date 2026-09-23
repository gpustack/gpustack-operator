package worker

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
)

func TestValidateTopologySource(t *testing.T) {
	ref := workercore.TopologySourceObjectReference{Namespace: kuberess.SystemNamespaceName, Name: "inventory"}
	valid := func() *workercore.TopologySource {
		return &workercore.TopologySource{Spec: workercore.TopologySourceSpec{
			NodeSelector: meta.LabelSelector{},
			Levels:       []string{"topology.kubernetes.io/region", "topology.kubernetes.io/zone"},
			NodeLabels:   &workercore.TopologySourceNodeLabels{},
		}}
	}

	tests := []struct {
		name  string
		edit  func(*workercore.TopologySource)
		valid bool
	}{
		{name: "node labels", edit: func(*workercore.TopologySource) {}, valid: true},
		{name: "additional write prefix", edit: func(s *workercore.TopologySource) {
			s.Spec.NodeLabels = nil
			s.Spec.ConfigMap = &workercore.TopologySourceConfigMap{ConfigMapRef: ref, Key: "topology.yaml", MaxStaleness: meta.Duration{Duration: time.Minute}}
			s.Spec.AdditionalWritePrefix = "datacenter.example/"
		}, valid: true},
		{name: "config map", edit: func(s *workercore.TopologySource) {
			s.Spec.NodeLabels = nil
			s.Spec.ConfigMap = &workercore.TopologySourceConfigMap{ConfigMapRef: ref, Key: "topology.yaml", MaxStaleness: meta.Duration{Duration: time.Minute}}
		}, valid: true},
		{name: "bearer webhook", edit: func(s *workercore.TopologySource) {
			s.Spec.NodeLabels = nil
			s.Spec.Webhook = &workercore.TopologySourceWebhook{URL: "https://inventory.example.test/snapshot", PollInterval: meta.Duration{Duration: time.Minute}, Timeout: meta.Duration{Duration: time.Second}, MaxStaleness: meta.Duration{Duration: time.Minute}, BearerTokenSecretRef: &ref}
		}, valid: true},
		{name: "no union arm", edit: func(s *workercore.TopologySource) { s.Spec.NodeLabels = nil }},
		{name: "two union arms", edit: func(s *workercore.TopologySource) {
			s.Spec.ConfigMap = &workercore.TopologySourceConfigMap{ConfigMapRef: ref, Key: "topology.yaml", MaxStaleness: meta.Duration{Duration: time.Minute}}
		}},
		{name: "hostname is implicit", edit: func(s *workercore.TopologySource) { s.Spec.Levels = []string{"kubernetes.io/hostname"} }},
		{name: "too many levels", edit: func(s *workercore.TopologySource) {
			s.Spec.Levels = make([]string, 17)
			for i := range s.Spec.Levels {
				s.Spec.Levels[i] = fmt.Sprintf("topology.example.test/level-%d", i)
			}
		}},
		{name: "maximum levels", edit: func(s *workercore.TopologySource) {
			s.Spec.Levels = make([]string, 16)
			for i := range s.Spec.Levels {
				s.Spec.Levels[i] = fmt.Sprintf("topology.example.test/level-%d", i)
			}
		}, valid: true},
		{name: "read only write prefix", edit: func(s *workercore.TopologySource) { s.Spec.AdditionalWritePrefix = "datacenter.example/" }},
		{name: "reserved write prefix", edit: func(s *workercore.TopologySource) {
			s.Spec.NodeLabels = nil
			s.Spec.ConfigMap = &workercore.TopologySourceConfigMap{ConfigMapRef: ref, Key: "topology.yaml", MaxStaleness: meta.Duration{Duration: time.Minute}}
			s.Spec.AdditionalWritePrefix = "fabric.topograph.run/"
		}},
		{name: "reserved write subdomain", edit: func(s *workercore.TopologySource) {
			s.Spec.NodeLabels = nil
			s.Spec.ConfigMap = &workercore.TopologySourceConfigMap{ConfigMapRef: ref, Key: "topology.yaml", MaxStaleness: meta.Duration{Duration: time.Minute}}
			s.Spec.AdditionalWritePrefix = "private.kubernetes.io/"
		}},
		{name: "empty levels", edit: func(s *workercore.TopologySource) { s.Spec.Levels = nil }},
		{name: "webhook has user info", edit: func(s *workercore.TopologySource) {
			s.Spec.NodeLabels = nil
			s.Spec.Webhook = &workercore.TopologySourceWebhook{URL: "https://token@inventory.example.test", PollInterval: meta.Duration{Duration: time.Minute}, Timeout: meta.Duration{Duration: time.Second}, MaxStaleness: meta.Duration{Duration: time.Minute}, BearerTokenSecretRef: &ref}
		}},
		{name: "webhook uses HTTP", edit: func(s *workercore.TopologySource) {
			s.Spec.NodeLabels = nil
			s.Spec.Webhook = &workercore.TopologySourceWebhook{URL: "http://inventory.example.test", PollInterval: meta.Duration{Duration: time.Minute}, Timeout: meta.Duration{Duration: time.Second}, MaxStaleness: meta.Duration{Duration: time.Minute}, BearerTokenSecretRef: &ref}
		}},
		{name: "webhook uses unsupported scheme", edit: func(s *workercore.TopologySource) {
			s.Spec.NodeLabels = nil
			s.Spec.Webhook = &workercore.TopologySourceWebhook{URL: "ftp://inventory.example.test", PollInterval: meta.Duration{Duration: time.Minute}, Timeout: meta.Duration{Duration: time.Second}, MaxStaleness: meta.Duration{Duration: time.Minute}, BearerTokenSecretRef: &ref}
		}},
		{name: "webhook has fragment", edit: func(s *workercore.TopologySource) {
			s.Spec.NodeLabels = nil
			s.Spec.Webhook = &workercore.TopologySourceWebhook{URL: "https://inventory.example.test/snapshot#credential", PollInterval: meta.Duration{Duration: time.Minute}, Timeout: meta.Duration{Duration: time.Second}, MaxStaleness: meta.Duration{Duration: time.Minute}, BearerTokenSecretRef: &ref}
		}},
		{name: "webhook has two credentials", edit: func(s *workercore.TopologySource) {
			s.Spec.NodeLabels = nil
			s.Spec.Webhook = &workercore.TopologySourceWebhook{URL: "https://inventory.example.test", PollInterval: meta.Duration{Duration: time.Minute}, Timeout: meta.Duration{Duration: time.Second}, MaxStaleness: meta.Duration{Duration: time.Minute}, BearerTokenSecretRef: &ref, TLSClientCertificateSecretRef: &ref}
		}},
		{name: "reference leaves worker namespace", edit: func(s *workercore.TopologySource) {
			s.Spec.NodeLabels = nil
			s.Spec.ConfigMap = &workercore.TopologySourceConfigMap{ConfigMapRef: workercore.TopologySourceObjectReference{Namespace: "other", Name: "inventory"}, Key: "topology.yaml", MaxStaleness: meta.Duration{Duration: time.Minute}}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			source := valid()
			tc.edit(source)
			assert.Equal(t, tc.valid, len(validateTopologySource(source)) == 0)
		})
	}
}

func TestValidateModelDeploymentRoleTopology(t *testing.T) {
	tests := []struct {
		level string
		valid bool
	}{
		{level: "topology.kubernetes.io/zone", valid: true},
		{level: "kubernetes.io/hostname"},
		{level: "not a label"},
	}
	for _, tc := range tests {
		t.Run(tc.level, func(t *testing.T) {
			md := &workercore.ModelDeployment{Spec: workercore.ModelDeploymentSpec{Roles: []workercore.ModelDeploymentRole{{Topology: &workercore.ModelDeploymentRoleTopology{RequiredLevel: tc.level}}}}}
			assert.Equal(t, tc.valid, len(validateModelDeploymentRoleTopology(md)) == 0)
		})
	}
}
