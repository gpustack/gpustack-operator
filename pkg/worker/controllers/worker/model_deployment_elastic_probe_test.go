package worker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The liveness gate is tested by running the command the probe actually emits against a real
// server. Asserting the rendered string would prove only that this file builds the text it builds,
// and the whole point of the gate is what the text does when an engine answers 503, so the text is
// executed. The one substitution is the address: the production address is asserted exactly first,
// and only then is the same program pointed at a test server, which is the sole difference between
// what is shipped and what runs here.

const elasticLivenessTestBudget = 20 * time.Second

// runElasticLivenessProgram executes the emitted program against url and reports the exit code.
//
// THE EXIT CODE IS THE WHOLE CONTRACT: the kubelet reads nothing else about an exec probe. A zero
// passes the gate and anything else restarts the engine, so the code is returned rather than a
// boolean, and a program that failed to run at all is a failure of the gate rather than a pass.
func runElasticLivenessProgram(t *testing.T, url string) int {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), elasticLivenessTestBudget)
	defer cancel()

	cmd := exec.CommandContext(ctx, "python3", "-c", modelDeploymentElasticLivenessScript(url))
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("the program did not answer within %s: %s", elasticLivenessTestBudget, output)
	}
	if err == nil {
		return 0
	}

	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("the program did not run: %v: %s", err, output)
	}

	return exit.ExitCode()
}

// elasticLivenessServer starts a server answering one status and returns its address. A hold
// delays the response past the program's own request bound, which is how a wedged engine behaves.
func elasticLivenessServer(t *testing.T, status int, hold time.Duration) string {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hold > 0 {
			time.Sleep(hold)
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(server.Close)

	return server.URL
}

// refusedAddress returns an address nothing is listening on, which is the answer an engine that
// has already died gives. The listener is closed before the address is used, so a connection to
// it is refused rather than queued.
func refusedAddress(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	require.NoError(t, listener.Close())

	return "http://" + address
}

// TestModelDeploymentElasticLivenessProbeShape pins the probe the render installs, because the
// gate is only native-aware if it is an exec: an HTTPGet probe cannot pass a 503, which is the
// whole reason this probe exists.
func TestModelDeploymentElasticLivenessProbeShape(t *testing.T) {
	probe := modelDeploymentElasticLivenessProbe()
	require.NotNil(t, probe)
	require.Nil(t, probe.HTTPGet, "an HTTPGet probe cannot accept the resize window")
	require.NotNil(t, probe.Exec)

	require.Equal(t, []string{"python3", "-c", modelDeploymentElasticLivenessScript(
		modelDeploymentElasticLivenessURL)}, probe.Exec.Command)
	require.Contains(t, probe.Exec.Command[2], "http://127.0.0.1:8000/health",
		"the probe must read the engine's own health address")
	require.Equal(t, modelDeploymentProbePeriodSeconds, probe.PeriodSeconds)
	require.Equal(t, modelDeploymentProbeTimeoutSeconds, probe.TimeoutSeconds)
	require.Equal(t, modelDeploymentLivenessFailureThreshold, probe.FailureThreshold)
}

// TestModelDeploymentElasticLivenessPredicate runs the emitted program against every answer the
// engine can give, and asserts which of them pass the gate.
//
// THE TWO THAT PASS ARE EXACTLY TWO. A 200 is a serving engine. A 503 is an engine that is alive
// and mid-resize, and refusing to restart it is the entire reason this program replaces the
// kubelet's own gate. Every other answer -- a 500, a 502, a 204 that is not the success this
// program means, a wedged response, a refused connection -- is a failure, because an engine that
// is gone or broken answers the same way and must be restarted.
func TestModelDeploymentElasticLivenessPredicate(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		hold     time.Duration
		refused  bool
		wantPass bool
	}{
		{name: "a serving engine answers 200", status: http.StatusOK, wantPass: true},
		{name: "a resizing engine answers 503", status: http.StatusServiceUnavailable, wantPass: true},
		{name: "an engine fault answers 500", status: http.StatusInternalServerError},
		{name: "an upstream fault answers 502", status: http.StatusBadGateway},
		{name: "a 204 is not the success this program means", status: http.StatusNoContent},
		{name: "a restarted engine answers 404", status: http.StatusNotFound},
		{name: "a wedged engine holds past the request bound", status: http.StatusOK, hold: 5 * time.Second},
		{name: "a dead engine refuses the connection", refused: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var url string
			if tc.refused {
				url = refusedAddress(t)
			} else {
				url = elasticLivenessServer(t, tc.status, tc.hold)
			}

			code := runElasticLivenessProgram(t, url+"/health")
			if tc.wantPass {
				require.Zero(t, code, "this answer must pass the liveness gate")
			} else {
				require.NotZero(t, code, "this answer must fail the liveness gate")
			}
		})
	}
}

// TestModelDeploymentElasticLivenessRejectsGatedStatusCodes keeps the accepted set from widening by
// accident. A predicate that accepts every code at or below 503 would also pass a 500, and the
// engine being healthy is the only claim either code makes.
func TestModelDeploymentElasticLivenessRejectsGatedStatusCodes(t *testing.T) {
	for _, status := range []int{400, 401, 404, 418, 429, 500, 502, 504} {
		t.Run(fmt.Sprintf("status=%d", status), func(t *testing.T) {
			code := runElasticLivenessProgram(t, elasticLivenessServer(t, status, 0)+"/health")
			require.NotZero(t, code, "only 200 and 503 may pass the liveness gate")
		})
	}
}

// TestModelDeploymentElasticLivenessBoundsEveryRead states the bound the probe carries, so a program
// that hangs cannot become a gate that hangs with it.
func TestModelDeploymentElasticLivenessBoundsEveryRead(t *testing.T) {
	probe := modelDeploymentElasticLivenessProbe()
	require.Positive(t, probe.TimeoutSeconds, "the kubelet must have a timeout to enforce")
	require.Positive(t, modelDeploymentElasticLivenessRequestSeconds,
		"the program's own request bound must be positive")
	require.Less(t, int(modelDeploymentElasticLivenessRequestSeconds), int(probe.TimeoutSeconds),
		"the program must answer before the kubelet kills it, or its exit code is never read")
}

// TestModelDeploymentElasticLivenessScriptIsBounded checks the two properties the shipped program
// must keep regardless of the answers around it: it reads no more than the engine's own health
// address, and it does not name a shell, a sidecar or a binary the image may not carry.
func TestModelDeploymentElasticLivenessScriptIsBounded(t *testing.T) {
	script := modelDeploymentElasticLivenessScript(modelDeploymentElasticLivenessURL)
	require.Contains(t, script, "urllib.request")
	require.Equal(t, 1, strings.Count(script, "http://"),
		"the program must reach exactly one address")
	require.NotContains(t, script, "sh -c")
}

// TestModelDeploymentElasticLivenessReleasesEveryResponseItOpens states that the shipped program
// releases every response it obtains, on the accepted-503 path as well as on the 200 path.
//
// The 503 case is the steady state for the whole resize window, and a program that accepts it
// obtains its response from an exception rather than from the call, so the release the success path
// performs has to be performed there too. The assertion is on the shipped text because no runtime
// observation can separate the two forms: the probe is a separate short-lived process per
// invocation, so nothing accumulates across invocations, and on this interpreter the error object is
// released when its scope ends whether or not the program says so. Measured on CPython 3.11 by
// counting the process's own descriptors after the program returns: identical with and without the
// release. The contract is what a reader has to be able to see, so the contract is what is asserted.
func TestModelDeploymentElasticLivenessReleasesEveryResponseItOpens(t *testing.T) {
	script := modelDeploymentElasticLivenessScript(modelDeploymentElasticLivenessURL)
	opened := strings.Count(script, "= urllib.request.urlopen(") + strings.Count(script, "as err:")
	require.Positive(t, opened)
	require.Equal(t, opened, strings.Count(script, ".close()"),
		"every response the program obtains must be released on the path that obtained it")
}
