package settings

import (
	"gpustack.ai/gpustack/pkg/setting"
)

var settings = setting.Settings{}

// Indexer returns the index function of the settings,
// which can be used to index the settings in other packages.
func Indexer() setting.IndexFunc {
	return settings.Index
}

// the built-in settings for server.
var (
	BootstrapPasswordProvisionState = settings.NewEditable(
		"bootstrap-password-provision-state",
		"Indicates the provision state of the bootstrap password.",
		setting.InitializeFrom("specified"),
		setting.Allow(),
	)
	ContainerRegistry = settings.NewEditable(
		"container-registry",
		"Indicates the registry to pull images.",
		setting.InitializeFromEnv(),
		setting.AllowBlank(),
		setting.AllowContainerRegistry(),
	)
	ContainerNamespace = settings.NewEditable(
		"container-namespace",
		"Indicates the namespace to pull images.",
		setting.InitializeFromEnv(),
		setting.AllowBlank(),
	)
	ImportLocalCluster = settings.NewEditable(
		"import-local-cluster",
		"Indicates whether to import the local cluster, "+
			"the local cluster is the Kubernetes cluster where the server is running. "+
			"Importing the local cluster allows the server to manage it, "+
			"but it may also introduce security risks if the local cluster is not properly secured.",
		setting.InitializeFromEnv("false"),
		setting.AllowBool(),
	)
	ServeUiUrl = settings.NewPrivate(
		"serve-ui-url",
		"Indicates a URL to provide the server UI, "+
			"it's in form of [https|file]://address[:port]/path.",
		setting.InitializeFromEnv(),
		setting.DisallowBlank(),
		setting.AllowUrlWithSchema("https", "file"),
	)
	ServeUrl = settings.NewEditable(
		"serve-url",
		"Indicates the URL to access the server, "+
			"it's in form of https://address[:port].",
		setting.InitializeFromEnv(),
		setting.DisallowBlank(),
		setting.AllowUrlWithSchema("https"),
	)
	SubjectLoginExpirationSeconds = settings.NewEditable(
		"subject-login-expiration-seconds",
		"Indicates the expiration seconds of the subject login, "+
			"it is also controlled by the loopback Kubernetes Cluster ApiServer. "+
			"The default is an hour, it can be configured to no larger than 24 hours. "+
			"Reasonable value should be provided, "+
			"so that the login can keep a balance between security and convenience.",
		setting.InitializeFrom("3600"),
		setting.DisallowBlank(),
		setting.AllowUint64InRange(3600, 3600*24),
	)
	SubjectTokenMaximumExpirationSeconds = settings.NewEditable(
		"subject-token-maximum-expiration-seconds",
		"Indicates the maximum expiration seconds of the subject token, "+
			"it is also controlled by the loopback Kubernetes Cluster ApiServer. "+
			"The default is 2 hours. "+
			"Reasonable value should be provided, "+
			"so that the token does not have an excessively long validity period.",
		setting.InitializeFrom("7200"),
		setting.DisallowBlank(),
		setting.AllowUint64(),
	)
)
