package exporter

import (
	"context"
	"time"

	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
)

type Config struct {
	// ClientReader reads this node's Instance pods. It must be backed by an informer carrying
	// deviceplugin.IndexingPodsByNodeName; when unset, New takes the controller runtime
	// pkg/manager configured.
	ClientReader ctrlcli.Reader

	// MonitorPeriod is how often this node's Instances are sampled.
	//
	// The device manager hands it the detector's period: the two publish one sample per period
	// on the same endpoint, and a consumer scaling its staleness bound to the period it is told
	// cannot be handed two different ones.
	MonitorPeriod time.Duration
}

func (c *Config) Apply(_ context.Context) (*Poller, error) {
	return New(c)
}
