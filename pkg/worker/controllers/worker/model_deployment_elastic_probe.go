package worker

import (
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
)

// The elastic master stays alive while it re-forms its collective, and the engine says so by
// answering 503 on every endpoint for the length of the resize. The kubelet's own HTTP gate does
// not read that: it treats anything outside 200-399 as a failure, so a resize long enough to reach
// the liveness threshold reads as a dead engine and the kubelet restarts the very process that is
// mid-resize. Withdrawing traffic is the gate's job during a resize, and readiness still does it.
//
// So the liveness gate is an exec of the image's own python3, which reads the status code itself
// and passes on exactly two answers: 200 for a serving engine, and 503 for one that is alive and
// re-forming. A refused connection, a timeout, or any other status is a failure, because an engine
// that is gone or broken gives those same answers and must be restarted.
const (
	// modelDeploymentElasticLivenessURL is the engine's own health address on the master
	// container, reached over the loopback interface so the gate never leaves the Pod.
	modelDeploymentElasticLivenessURL = "http://127.0.0.1:8000/health"

	// modelDeploymentElasticLivenessRequestSeconds bounds one read inside the program. It is below
	// the probe's kubelet timeout so the program answers first and its exit code is what the
	// kubelet reads, rather than a kill that arrives before the code does.
	modelDeploymentElasticLivenessRequestSeconds = 1
)

// modelDeploymentElasticLivenessProbe is the liveness gate the master container runs. It is the
// only one of the three that changes: startup still waits for the engine to load, and readiness
// still withholds traffic for the whole resize window.
//
// It reads the engine directly and takes no option from the spec, so a deployment cannot widen the
// accepted set into a gate that never restarts a broken engine.
func modelDeploymentElasticLivenessProbe() *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{
				Command: []string{
					"python3", "-c",
					modelDeploymentElasticLivenessScript(modelDeploymentElasticLivenessURL),
				},
			},
		},
		PeriodSeconds:    modelDeploymentProbePeriodSeconds,
		TimeoutSeconds:   modelDeploymentProbeTimeoutSeconds,
		FailureThreshold: modelDeploymentLivenessFailureThreshold,
	}
}

// modelDeploymentElasticLivenessScript renders the program. It takes the address as an argument so
// a test can point the same program at a test server; the probe passes the production one.
//
// HTTPError is caught before the general case because it is a subclass of URLError, and the 503
// this file exists to accept arrives as an exception rather than as a response. The code is read
// from whichever attribute the interpreter's response object carries, so an older image that
// exposes only code is read the same way. That object is a response too, so it is closed the same
// way the success path closes its own, and every path that opened a connection releases it.
//
// A program that cannot run is a failed gate, not a passing one: every path that does not learn a
// code exits non-zero, and the accepted set is written out rather than implied by a range.
func modelDeploymentElasticLivenessScript(url string) string {
	return fmt.Sprintf(`import sys, urllib.error, urllib.request
try:
    response = urllib.request.urlopen(%[1]s, timeout=%[2]d)
    code = getattr(response, "status", None)
    if code is None:
        code = response.getcode()
    response.close()
except urllib.error.HTTPError as err:
    code = err.code
    err.close()
except Exception:
    sys.exit(1)
sys.exit(0 if code in (200, 503) else 1)
`, strconv.Quote(url), modelDeploymentElasticLivenessRequestSeconds)
}
