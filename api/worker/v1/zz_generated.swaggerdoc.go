package v1

func (InstanceLogOptions) SwaggerDoc() map[string]string {
	return map[string]string{
		"":             "InstanceLogOptions is the options of Instance log request, which is the same as Kubernetes PodLogOptions.",
		"follow":       "Follow the log stream of the Instance.",
		"sinceSeconds": "A relative time in seconds before the current time from which to show logs. If this value precedes the time an Instance was started, only logs since the Instance start will be returned. If this value is in the future, no logs will be returned. Only one of sinceSeconds or sinceTime may be specified.",
		"sinceTime":    "An RFC3339 timestamp from which to show logs. If this value precedes the time an Instance was started, only logs since the Instance start will be returned. If this value is in the future, no logs will be returned. Only one of sinceSeconds or sinceTime may be specified.",
		"timestamps":   "If true, add an RFC3339 or RFC3339Nano timestamp at the beginning of every line of log output.",
		"tailLines":    "If set, the number of lines from the end of the logs to show. If not specified, logs are shown from the creation of the container or sinceSeconds or sinceTime.",
		"limitBytes":   "If set, the number of bytes to read from the server before terminating the log output. This may not display a complete final line of logging, and may return slightly more or slightly less than the specified limit.",
	}
}
