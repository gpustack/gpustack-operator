package system

import (
	"k8s.io/apimachinery/pkg/util/sets"

	"gpustack.ai/gpustack/pkg/utils/varx"
)

var (
	// BootstrapPassword is the password for bootstrapping the system.
	BootstrapPassword = varx.NewOnce("")

	// DisableAuths is a flag to disable authentication.
	DisableAuths = varx.NewOnce(false)

	// DisableApplications is a set of applications that are not allowed to be installed.
	DisableApplications = varx.NewOnce(sets.New[string]())

	// ModelManagerServiceAccount is the ServiceAccount, in the system namespace, the model-manager
	// plugin runs as: the only identity that may write a NodeModelStore's status.
	ModelManagerServiceAccount = varx.NewOnce("gpustack-operator-model-manager")
)

// ConfigureControl configures the function of the system.
func ConfigureControl(
	bootstrapPassword string,
	disableAuths bool,
	disableApps []string,
) {
	BootstrapPassword.Configure(bootstrapPassword)
	DisableAuths.Configure(disableAuths)
	DisableApplications.Configure(sets.New[string](disableApps...))
}
