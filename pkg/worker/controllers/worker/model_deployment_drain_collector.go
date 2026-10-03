package worker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// THE TRANSPORT IS AN EXEC, NOT A SCRAPE, because the question is per member and only the member
// can answer it. A scrape would arrive through the same Service the router uses, and a Service
// load balances across the members, so it cannot say which one is idle. An exec runs inside the
// exact container, on that container's loopback, and the answer is that member's and no other's.
//
// It is the read-only twin of the preStop hook, and it is deliberately a second READING of the same
// gauges rather than a reuse of the hook: the hook runs on the kubelet's grace clock after the
// delete, which is too late to decide anything, and its answer is deliberately unreportable because
// it always exits zero. This read's answer is acted on, so it carries its own envelope and its own
// completeness, and a parse failure has to be visible rather than absorbed.
const (
	// modelDeploymentDrainHTTPTimeoutSeconds bounds the engine's own answer. The hook allows one
	// second on the kubelet's clock; a controller-side read is not on that clock, so it can afford
	// one more and still return inside a reconcile pass.
	modelDeploymentDrainHTTPTimeoutSeconds = 2

	// modelDeploymentDrainExecTimeoutSeconds bounds the whole read, dial included. Two reads of one
	// member cost at most this twice, and both are holds, so the pass returns rather than blocks.
	modelDeploymentDrainExecTimeoutSeconds = 5

	// modelDeploymentDrainMaxBodyBytes caps what one read may pull. An engine whose endpoint
	// misbehaves is bounded here rather than by the size of whatever it chooses to serve.
	modelDeploymentDrainMaxBodyBytes = 1 << 20
)

// modelDeploymentDrainIndexPodUID indexes a Pod by its own UID.
//
// THE TARGET IS A UID AND NOT A NAME because a name is not an identity here. A replica is created
// with GenerateName and nothing else (model_deployment_render.go), so its name is whatever the API
// server chose, and a Pod deleted and recreated under the same name would answer for a member the
// reservation never held. The UID is the one handle that cannot be reused.
const modelDeploymentDrainIndexPodUID = "gpustack.ai/model-deployment-drain-pod-uid"

// modelDeploymentDrainExec runs one command inside one container and returns its standard output.
//
// It is a function rather than a dials this reader makes, for the reason the seam is an interface:
// the answers this has to get right are failures, and a real SPDY stream cannot be made to produce
// a refused exec, a truncated body or a container that changed identity mid-read on demand.
type modelDeploymentDrainExec func(
	ctx context.Context, pod *corev1.Pod, container string, argv []string,
) (string, error)

// modelDeploymentDrainCollector is the production reader: it resolves one member's Pod, checks
// that the member is one this protocol is allowed to measure, and reads its engine's own gauges.
//
// IT HOLDS NO STATE BETWEEN READS. Continuity is proven from the live Pod rather than from a
// remembered sample, so a controller restart cannot lose it and there is no mutex to get wrong.
// The cost of that choice is stated where it is paid, at the restart check below.
type modelDeploymentDrainCollector struct {
	// client locates the target Pod. Its field index is a LOCATOR and not an authority: it answers
	// which namespace and name carry a member UID, and the object it returns is the one the
	// controller's cache last saw, which may predate a restart, a replacement or a relabel.
	client ctrlcli.Client

	// fresh is the authority the locator's answer is checked against. Every fact this read acts on
	// is taken from a live read rather than from the cache, and it is taken twice, once before the
	// exec and once after, because a container can be replaced under a stable Pod while the stream
	// is open. A nil fresh reader is a hold rather than a fallback to the cache: a read that cannot
	// prove what it is measuring has not measured it.
	fresh ctrlcli.Reader

	// restConfig and clientset carry the exec. They are separate because the REST client is what
	// builds the subresource URL and the executor is what upgrades it to a stream.
	// core is the typed core client rather than the whole clientset, because the only subresource
	// this reader touches is pods/exec. Pulling the full clientset for one verb would add a
	// dependency surface the operator has no other reason to carry.
	core       corev1client.CoreV1Interface
	restConfig *rest.Config

	exec modelDeploymentDrainExec
}

// newModelDeploymentDrainCollector builds the production reader. A nil exec installs the real one,
// so a test can supply its own and a production caller cannot accidentally get a nil.
func newModelDeploymentDrainCollector(
	client ctrlcli.Client, fresh ctrlcli.Reader, restConfig *rest.Config,
	core corev1client.CoreV1Interface,
) *modelDeploymentDrainCollector {
	collector := &modelDeploymentDrainCollector{
		client:     client,
		fresh:      fresh,
		restConfig: restConfig,
		core:       core,
	}

	collector.exec = collector.execInto

	return collector
}

// Drain reports what one member still holds in flight.
//
// THE ORDER OF THE CHECKS IS THE ORDER OF WHAT MUST BE PROVEN, and each one that fails is a hold
// rather than an answer: the target is this member, the member is measurable at all, the member is
// one this protocol may act on, the engine it runs is one with a gauge list, the container is the
// same instance before and after, and only then is a number read and believed.
//
// NOT EVERY FAILURE IS AN ERROR. A transport that cannot answer returns an answer carrying the
// shape of what went wrong, because the protocol's reason string is what tells an operator why a
// replica is being held. Returning an error instead would collapse all of them into one line and
// leave the hold unexplained.
func (c *modelDeploymentDrainCollector) Drain(
	ctx context.Context, target modelDeploymentDrainTarget,
) (modelDeploymentDrainAnswer, error) {
	if target.PodUID == "" {
		// An empty UID matches no member and, worse, could match every one of them if the lookup
		// were written as a filter rather than an equality. It fails closed.
		return modelDeploymentDrainHold(modelDeploymentDrainUnknown,
			"the drain target names no member"), nil
	}

	if target.Container == "" {
		return modelDeploymentDrainHold(modelDeploymentDrainUnknown,
			"the drain target names no container to read"), nil
	}

	expected, known := modelDeploymentInFlightMetrics[target.Engine]
	if !known {
		// Same refusal the protocol itself makes, and for the same reason: an engine with no gauge
		// list has no measurement, and inventing one here would be the assumption Unsupported
		// exists to refuse.
		return modelDeploymentDrainHold(modelDeploymentDrainUnsupported,
			"engine "+target.Engine+" has no in-flight gauge list"), nil
	}

	pod, hold := c.resolveMember(ctx, target, "resolved")
	if hold != nil {
		return *hold, nil
	}

	container, found := modelDeploymentDrainContainerOf(pod, target.Container)
	if !found {
		// The seam carries the container rather than looking it up, and the carried name is still
		// only a claim: the Pod is the authority on which containers it has. A claim the Pod
		// contradicts is not a container this read may enter.
		return modelDeploymentDrainHold(modelDeploymentDrainUnknown,
			fmt.Sprintf("member %s has no container %q", pod.Name, target.Container)), nil
	}

	if reason, disaggregated := modelDeploymentDrainDisaggregated(target.Engine, container); disaggregated {
		return modelDeploymentDrainHold(modelDeploymentDrainUnsupported, reason), nil
	}

	if drift := modelDeploymentDrainExpectedIdentity(pod, target, target.Container); drift != "" {
		return modelDeploymentDrainHold(modelDeploymentDrainUnknown, drift), nil
	}

	// The first live read is kept whole rather than reduced to a container status, because the
	// comparison after the exec is against the member as it was and not against one field of it.
	beforePod := pod
	before, ok := modelDeploymentDrainContainerStatus(beforePod, target.Container)
	if !ok {
		return modelDeploymentDrainHold(modelDeploymentDrainUnknown,
			fmt.Sprintf("member %s reports no status for container %q", pod.Name, target.Container)), nil
	}

	// A RESTART IS NOT INDEFINITE AND IT IS NOT FREE. The gauges now served come from an engine
	// that did not exist when the protocol began watching, so they cannot speak for the work the
	// protocol set out to account for. The hold is bounded by the drain budget, which ends the
	// retirement as Aborted with the member retained rather than deleted, so a member that keeps
	// crashing is never deleted and never blocks the queue forever.
	if before.RestartCount > 0 {
		return modelDeploymentDrainHold(modelDeploymentDrainUnknown,
			fmt.Sprintf("member %s container %q has restarted %d time(s) since it was created",
				pod.Name, target.Container, before.RestartCount)), nil
	}

	url, ok := modelDeploymentDrainURL(pod)
	if !ok {
		return modelDeploymentDrainHold(modelDeploymentDrainUnknown,
			fmt.Sprintf("member %s carries no usable metrics endpoint annotation", pod.Name)), nil
	}

	body, err := c.read(ctx, pod, target.Container, url, expected)
	if err != nil {
		return modelDeploymentDrainHold(modelDeploymentDrainUnknown, err.Error()), nil
	}

	// THE POST-CHECK IS WHAT MAKES THE READ ABOUT THIS MEMBER. A container can be replaced under a
	// stable Pod between the resolve and the exec, so its identity is re-read and compared rather
	// than assumed to have held.
	//
	// THE MEMBER'S OWN IDENTITY IS NOT COMPARED HERE, because the re-resolve is a lookup BY that
	// UID through an index built from each Pod's own UID: a member that was deleted and replaced
	// carries a new one, so the lookup finds nothing and the read is already unobserved. A
	// comparison here could not fail, and a check that cannot fail reads like a protection that
	// does not exist.
	after, hold := c.resolveMember(ctx, target, "re-resolved after the read")
	if hold != nil {
		return *hold, nil
	}
	afterStatus, ok := modelDeploymentDrainContainerStatus(after, target.Container)
	if !ok {
		return modelDeploymentDrainHold(modelDeploymentDrainUnknown,
			fmt.Sprintf("member %s reported no status for container %q after the read",
				after.Name, target.Container)), nil
	}
	if afterStatus.ContainerID != before.ContainerID {
		return modelDeploymentDrainHold(modelDeploymentDrainUnknown,
			fmt.Sprintf("container %q was replaced while it was being read", target.Container)), nil
	}

	// THE TWO BOOKENDS MUST BE ABOUT THE SAME MEMBER, and a container ID is the smallest part of
	// that. Everything else this read acted on is compared here as well, because each is a way the
	// member under the read can stop being the member the reservation bound: an owner that changed
	// is a Pod that has been adopted, a role that changed is a member re-seated or relabelled, an
	// address that changed is a member that has moved, and a metrics endpoint that changed is a
	// different process being read. A read whose subject moved cannot answer about the work the
	// protocol is accounting for, whatever the numbers say.
	if drift := modelDeploymentDrainIdentityDrift(beforePod, after, target.Container); drift != "" {
		return modelDeploymentDrainHold(modelDeploymentDrainUnknown, drift), nil
	}

	return modelDeploymentDrainAnswerFromBody(body, expected), nil
}

// modelDeploymentDrainIdentityDrift names what changed about the member between the two live reads,
// or the empty string when nothing this read depends on did.
//
// THE ROLE IS COMPARED AGAINST THE TARGET AND NOT ONLY AGAINST ITSELF, because a role that changed
// from one value to another is still a change, and a role that is not the one the reservation names
// is a member this operation never bound. The label is read through the tree's own accessor rather
// than by a key written here, so there is one answer to "which role is this Pod" in the tree.
func modelDeploymentDrainIdentityDrift(
	before, after *corev1.Pod, container string,
) string {
	if before.UID != after.UID {
		return fmt.Sprintf("member %s was replaced while it was being read", after.Name)
	}

	if owner := modelDeploymentDrainControllerOf(before); owner != modelDeploymentDrainControllerOf(after) {
		return fmt.Sprintf("member %s changed controller while it was being read", after.Name)
	}

	if role := modelDeploymentPodRole(after); role != modelDeploymentPodRole(before) {
		return fmt.Sprintf("member %s changed role from %q to %q while it was being read",
			after.Name, modelDeploymentPodRole(before), role)
	}

	if before.Status.PodIP != after.Status.PodIP {
		return fmt.Sprintf("member %s changed address while it was being read", after.Name)
	}

	if before.Annotations["prometheus.io/port"] != after.Annotations["prometheus.io/port"] ||
		before.Annotations["prometheus.io/scheme"] != after.Annotations["prometheus.io/scheme"] ||
		before.Annotations["prometheus.io/path"] != after.Annotations["prometheus.io/path"] {
		return fmt.Sprintf("member %s changed its metrics endpoint while it was being read", after.Name)
	}

	beforeStatus, beforeFound := modelDeploymentDrainContainerStatus(before, container)
	afterStatus, afterFound := modelDeploymentDrainContainerStatus(after, container)
	if !beforeFound || !afterFound {
		return fmt.Sprintf("member %s reported no status for container %q across the read",
			after.Name, container)
	}

	// A restart count that moved is a restart that happened during the read, and the gauges now
	// served come from a process that did not exist when the read began.
	if beforeStatus.RestartCount != afterStatus.RestartCount {
		return fmt.Sprintf("container %q restarted while it was being read", container)
	}

	return ""
}

// modelDeploymentDrainControllerOf is the Pod's controlling owner, which is what binds a replica to
// the deployment that declared it.
// modelDeploymentDrainControllerOf is the Pod's controlling owner as an identity.
//
// THE UID IS PART OF IT, and leaving it out made the comparison vacuous in two ways at once: a Pod
// whose owner was deleted and recreated under the same name compared equal to itself, and two Pods
// with no owner at all compared equal to each other. A name is not an identity for the same reason
// a member's name is not.
func modelDeploymentDrainControllerOf(pod *corev1.Pod) string {
	owner := metav1.GetControllerOf(pod)
	if owner == nil {
		return ""
	}

	return owner.Kind + "/" + owner.Name + "/" + string(owner.UID)
}

// modelDeploymentDrainExpectedIdentity is what the live Pod must still be for this read to be about
// the member the reservation bound.
//
// EVERY FIELD IS CHECKED BECAUSE EVERY ONE CAN BE ABSENT. An empty expected value and an empty
// observed value are not agreement, so a target that names no deployment, or a live Pod with no
// controller, is refused rather than passed. A read that cannot say which deployment it is measuring
// is a read of nothing in particular.
func modelDeploymentDrainExpectedIdentity(
	pod *corev1.Pod, target modelDeploymentDrainTarget, container string,
) string {
	if target.DeploymentUID == "" {
		return "the read names no deployment to check the member against"
	}

	controller := modelDeploymentDrainControllerOf(pod)
	if controller == "" {
		return fmt.Sprintf("member %s has no controlling owner", pod.Name)
	}
	if !strings.HasSuffix(controller, "/"+string(target.DeploymentUID)) {
		return fmt.Sprintf("member %s is owned by %s rather than the operation's deployment %s",
			pod.Name, controller, target.DeploymentUID)
	}

	// The role is read through the tree's own accessor, so there is one answer to which role a Pod
	// is rather than two that can drift.
	if role := modelDeploymentPodRole(pod); role != target.Role {
		return fmt.Sprintf("member %s is role %q rather than %q", pod.Name, role, target.Role)
	}

	status, found := modelDeploymentDrainContainerStatus(pod, container)
	if !found {
		return fmt.Sprintf("member %s reports no status for container %q", pod.Name, container)
	}
	// AN EMPTY CONTAINER IDENTITY IS NOT A MATCHING ONE. The kubelet has not yet reported which
	// container this is, and two empty identities agree with each other while saying nothing.
	if status.ContainerID == "" {
		return fmt.Sprintf("member %s reports no container identity for %q", pod.Name, container)
	}

	return ""
}

// read runs the collector script and returns whatever envelope it managed to produce.
//
// A TRUNCATED BODY IS NOT AN ERROR, because the script is allowed to have found nothing and still
// exit zero. An incomplete envelope is what an absent or half-read answer looks like, and the
// caller turns it into a hold; treating it as a transport error here would say less about it.
func (c *modelDeploymentDrainCollector) read(
	ctx context.Context, pod *corev1.Pod, container, url string, expected []string,
) (string, error) {
	readCtx, cancel := context.WithTimeout(ctx, modelDeploymentDrainExecTimeoutSeconds*time.Second)
	defer cancel()

	return c.exec(readCtx, pod, container, modelDeploymentDrainArgv(url, expected))
}

// resolveMember is the one lookup, and it returns a Pod or the hold that explains its absence rather
// than an error, because a member that cannot be found is not a failure of the transport: it is one
// of the answers this read is built to give, and it has to reach the operator's status with the reason
// intact.
//
// The lookup is by UID through a field index rather than a list, so a read costs one cache lookup
// instead of a sweep of every Pod in the cluster. It refuses when more than one Pod carries the UID,
// which cannot happen and is refused rather than resolved to the first of them.
// THE CACHE LOCATES, THE SERVER AUTHORIZES. The index is how this reader learns which namespace
// and name carry a member UID, because a UID is an identity and a name is not; it is not how the
// read is answered, because the cached object is whatever the watch last delivered and a member that
// has restarted, been replaced or been relabelled since then is still in it. The name the locator
// found is therefore used to read the object live, and the live object's own UID is matched against
// the frozen target, because a Pod deleted and recreated under the same name answers with a new UID
// and is a different member.
//
// A Pod UID field selector would read more cheaply and would be refused: the API server does not
// index that field, so a reader that used one would depend on a scan it cannot ask for.
func (c *modelDeploymentDrainCollector) resolveMember(
	ctx context.Context, target modelDeploymentDrainTarget, phase string,
) (*corev1.Pod, *modelDeploymentDrainAnswer) {
	pods := &corev1.PodList{}
	if err := c.client.List(ctx, pods,
		ctrlcli.MatchingFields{modelDeploymentDrainIndexPodUID: string(target.PodUID)},
	); err != nil {
		hold := modelDeploymentDrainHold(modelDeploymentDrainUnknown,
			fmt.Sprintf("member %s could not be %s: %v", phase, "looked up", err))

		return nil, &hold
	}

	switch len(pods.Items) {
	case 0:
		hold := modelDeploymentDrainHold(modelDeploymentDrainUnknown,
			fmt.Sprintf("member could not be %s: no pod carries member uid %s", phase, target.PodUID))

		return nil, &hold
	default:
		if len(pods.Items) > 1 {
			hold := modelDeploymentDrainHold(modelDeploymentDrainUnknown,
				fmt.Sprintf("member could not be %s: %d pods carry member uid %s",
					phase, len(pods.Items), target.PodUID))

			return nil, &hold
		}
	}

	if c.fresh == nil {
		hold := modelDeploymentDrainHold(modelDeploymentDrainUnknown,
			fmt.Sprintf("member could not be %s: no live reader is configured to confirm it", phase))

		return nil, &hold
	}

	key := ctrlcli.ObjectKeyFromObject(&pods.Items[0])
	pod := new(corev1.Pod)
	if err := c.fresh.Get(ctx, key, pod); err != nil {
		hold := modelDeploymentDrainHold(modelDeploymentDrainUnknown,
			fmt.Sprintf("member %s could not be %s: %v", phase, "read live", err))

		return nil, &hold
	}

	// The name the locator reported may now belong to another member entirely. A live read that
	// does not carry the frozen UID is a different Pod, whatever its name.
	if pod.UID != target.PodUID {
		hold := modelDeploymentDrainHold(modelDeploymentDrainUnknown,
			fmt.Sprintf("member could not be %s: %s/%s now carries uid %s rather than %s",
				phase, pod.Namespace, pod.Name, pod.UID, target.PodUID))

		return nil, &hold
	}

	return pod, nil
}

// execInto is the real transport: an SPDY stream into the container, bounded on both time and size.
//
// The command is the image's own python3, for the reason the preStop hook uses it: both engines are
// Python programs, so the interpreter is there, and inventing a second binary dependency for a
// read would be a new thing to trust. Standard error is not streamed, so an engine that talks
// cannot grow the response.
func (c *modelDeploymentDrainCollector) execInto(
	ctx context.Context, pod *corev1.Pod, container string, argv []string,
) (string, error) {
	if c.core == nil || c.restConfig == nil {
		return "", fmt.Errorf("no exec transport is configured")
	}

	request := c.core.RESTClient().
		Post().
		Resource("pods").
		Name(pod.Name).
		Namespace(pod.Namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   argv,
			Stderr:    false,
			TTY:       false,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(c.restConfig, "POST", request.URL())
	if err != nil {
		return "", fmt.Errorf("prepare exec into %s/%s: %w", pod.Namespace, pod.Name, err)
	}

	// The bound is enforced while the stream is running rather than after it, so an endpoint that
	// keeps writing is cut off at the cap instead of being allowed to grow the response and then
	// measured. Standard error is discarded into the same bounded writer: an engine that talks must
	// not be able to consume the budget the answer needs.
	stdout := &modelDeploymentDrainBounded{limit: modelDeploymentDrainMaxBodyBytes}
	if err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: stdout,
		Stderr: io.Discard,
	}); err != nil {
		return "", fmt.Errorf("exec into %s/%s: %w", pod.Namespace, pod.Name, err)
	}

	if stdout.exceeded {
		return "", fmt.Errorf("member %s served more than the %d byte bound",
			pod.Name, modelDeploymentDrainMaxBodyBytes)
	}

	return stdout.body.String(), nil
}

// modelDeploymentDrainBounded accumulates a stream up to a fixed size and remembers that it did not
// fit. It stops accepting bytes rather than truncating, because a truncated Prometheus body can end
// mid-line and parse as a shorter but well-formed answer.
type modelDeploymentDrainBounded struct {
	body     bytes.Buffer
	limit    int
	exceeded bool
}

// Write accumulates until the limit and refuses everything after it.
func (b *modelDeploymentDrainBounded) Write(p []byte) (int, error) {
	if b.body.Len()+len(p) > b.limit {
		b.exceeded = true
		return 0, fmt.Errorf("metrics body exceeds %d bytes", b.limit)
	}

	return b.body.Write(p)
}

// modelDeploymentDrainArgv is the one command this transport ever runs: the image's python3 with
// a generated program. There is no second form of it and no argument a user can reach, which is
// what makes the read-only claim checkable by reading this function.
func modelDeploymentDrainArgv(url string, expected []string) []string {
	names := make([]string, 0, len(expected))
	for _, name := range expected {
		names = append(names, strconv.Quote(name))
	}

	// THE SCRIPT SUMS ACROSS LABEL SETS AND NEVER ACROSS BODIES. A series the engine splits by
	// label, which both engines do, is one member's work reported in several rows, and the rows
	// that belong to it are the ones in this one body. Reading more than one body is how a
	// per-member zero turns into a group one, and there is no second read here to do it with.
	//
	// A sample it cannot parse is reported as unreadable rather than dropped, so a name this read
	// asked for and could not understand reaches the caller as a broken envelope instead of as an
	// absent one. Dropping it would leave a partial sum looking like a complete zero.
	// A REQUIRED SAMPLE IS NEVER DROPPED AND NEVER CANCELED. A gauge the engine splits by label
	// is several rows, and a row this script cannot read is still that gauge being reported and
	// missed; summing the sibling it did read would report a number the engine never wrote, and
	// letting the sibling stand would report a total the engine never meant. A value that is not a
	// count, or is not finite, or is below zero, refuses the gauge the same way an unreadable row
	// does, so there is no path by which bad evidence becomes a zero.
	script := fmt.Sprintf(`import math, sys, urllib.request
names = [%s]
try:
    body = urllib.request.urlopen(%s, timeout=%d).read(%d).decode("utf-8", "replace")
except Exception:
    sys.exit(0)
totals = {}
refused = set()
for line in body.splitlines():
    if not line or line.startswith("#"):
        continue
    name = line.split("{", 1)[0].split(" ", 1)[0]
    if name not in names:
        continue
    sample = line.rsplit("}", 1)[1] if "{" in line else line[len(name):]
    fields = sample.split()
    if not fields:
        refused.add(name)
        continue
    try:
        value = float(fields[0])
    except ValueError:
        refused.add(name)
        continue
    if not math.isfinite(value) or value < 0.0:
        refused.add(name)
        continue
    total = totals.get(name, 0.0) + value
    if not math.isfinite(total):
        refused.add(name)
        continue
    totals[name] = total
for name in sorted(refused):
    sys.stdout.write("%%s\tunreadable\n" %% name)
for name in sorted(totals):
    if name in refused:
        continue
    # repr round-trips the value exactly. A fixed format would round a count that is small but
    # real down to a zero, and a queue holding one request would be reported as holding none.
    sys.stdout.write("%%s\t%%s\n" %% (name, repr(totals[name])))
sys.exit(0)
`,
		strings.Join(names, ", "),
		strconv.Quote(url),
		modelDeploymentDrainHTTPTimeoutSeconds,
		modelDeploymentDrainMaxBodyBytes,
	)

	return []string{"python3", "-c", script}
}

// modelDeploymentDrainAnswerFromBody turns the envelope into the four states.
//
// COMPLETENESS IS DECIDED BEFORE THE NUMBERS ARE SUMMED, and a missing gauge is a hold rather than
// a zero. That ordering is the whole point: a reader that scraped two of the four SGLang gauges
// would otherwise sum what it found and report the emptiness of a series it never read, and the
// protocol would act on it.
//
// The protocol re-checks the same list independently against the one map in the tree, so this
// function is the first gate and not the only one.
func modelDeploymentDrainAnswerFromBody(
	body string, expected []string,
) modelDeploymentDrainAnswer {
	series := make(map[string]float64, len(expected))
	unparsable := make([]string, 0, len(expected))

	for _, line := range strings.Split(body, "\n") {
		if line == "" {
			continue
		}

		name, sample, found := strings.Cut(line, "\t")
		if !found {
			continue
		}
		value, err := strconv.ParseFloat(strings.TrimSpace(sample), 64)
		if err != nil || !modelDeploymentDrainActivityValue(value) {
			// A name this transport asked for, carrying something that is not a usable count, is a
			// broken envelope rather than an absent one. It is recorded so the reason says which.
			// Parsing into a float succeeds for NaN and the infinities, so the value is judged as
			// well as parsed, and a negative count is refused for the same reason.
			if modelDeploymentDrainExpected(name, expected) {
				unparsable = append(unparsable, name)
			}
			continue
		}
		series[name] = value
	}

	// A name that came back with something other than a number is reported before a name that did
	// not come back at all. Both hold, but they are different faults, and the engine that served an
	// unreadable value has answered while the engine that served nothing has not.
	if len(unparsable) > 0 {
		sort.Strings(unparsable)
		return modelDeploymentDrainAnswer{
			State:  modelDeploymentDrainUnknown,
			Reason: "the engine served unreadable values for " + strings.Join(unparsable, ", "),
		}
	}

	if missing := modelDeploymentDrainAbsentSeries(expected, series); len(missing) > 0 {
		return modelDeploymentDrainAnswer{
			State:  modelDeploymentDrainUnknown,
			Reason: "the engine served no " + strings.Join(missing, ", "),
		}
	}

	total := 0.0
	for _, value := range series {
		total += value
	}
	// The values are each finite and nonnegative, so the only way the sum stops being a count is an
	// overflow. It is refused rather than reported, because an engine serving counts this large is
	// not describing a queue this protocol can reason about.
	if math.IsInf(total, 0) || math.IsNaN(total) {
		return modelDeploymentDrainAnswer{
			State:  modelDeploymentDrainUnknown,
			Reason: "the engine's in-flight total is not a finite count",
		}
	}
	if total > 0 {
		return modelDeploymentDrainAnswer{
			State:    modelDeploymentDrainBusy,
			Reason:   fmt.Sprintf("the engine holds %.0f in flight", total),
			Complete: true,
			Series:   series,
		}
	}

	return modelDeploymentDrainAnswer{
		State:    modelDeploymentDrainIdle,
		Reason:   "the engine holds nothing in flight",
		Complete: true,
		Series:   series,
	}
}

// modelDeploymentDrainAbsentSeries names the gauges the envelope did not carry.
func modelDeploymentDrainAbsentSeries(expected []string, series map[string]float64) []string {
	missing := make([]string, 0, len(expected))
	for _, name := range expected {
		if _, present := series[name]; !present {
			missing = append(missing, name)
		}
	}

	return missing
}

// modelDeploymentDrainExpected reports whether a name is one this read was asked about.
func modelDeploymentDrainExpected(name string, expected []string) bool {
	for _, candidate := range expected {
		if candidate == name {
			return true
		}
	}

	return false
}

// modelDeploymentDrainHold is a hold carrying why, with completeness false so the protocol cannot
// read a number out of it whatever state it is given.
func modelDeploymentDrainHold(
	state modelDeploymentDrainState, reason string,
) modelDeploymentDrainAnswer {
	return modelDeploymentDrainAnswer{State: state, Reason: reason}
}

// modelDeploymentDrainContainerOf finds the container a read is about.
//
// The name is carried by the target rather than looked up, but a carried name is a claim and the
// Pod is the authority, so the claim is checked here rather than at the call site.
func modelDeploymentDrainContainerOf(
	pod *corev1.Pod, name string,
) (*corev1.Container, bool) {
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == name {
			return &pod.Spec.Containers[i], true
		}
	}

	return nil, false
}

// modelDeploymentDrainContainerStatus is the container's identity as the kubelet last saw it.
func modelDeploymentDrainContainerStatus(
	pod *corev1.Pod, name string,
) (*corev1.ContainerStatus, bool) {
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == name {
			return &pod.Status.ContainerStatuses[i], true
		}
	}

	return nil, false
}

// modelDeploymentDrainURL is the engine's own metrics endpoint as the Pod records it.
//
// IT IS READ FROM THE POD RATHER THAN DERIVED, for the reason the engine is read from the
// deployment rather than the role: a replica whose role the spec has already dropped is still
// drainable, and the role is exactly what may be gone. The annotations are written at render and
// carry the port the engine actually bound, which a role's own port declaration can disagree with.
func modelDeploymentDrainURL(pod *corev1.Pod) (string, bool) {
	port, err := strconv.Atoi(pod.Annotations["prometheus.io/port"])
	if err != nil || port <= 0 || port > 65535 {
		return "", false
	}

	schemeName := pod.Annotations["prometheus.io/scheme"]
	if schemeName != "http" && schemeName != "https" {
		return "", false
	}

	path := pod.Annotations["prometheus.io/path"]
	if path == "" {
		path = "/metrics"
	}
	if !strings.HasPrefix(path, "/") {
		return "", false
	}

	return fmt.Sprintf("%s://127.0.0.1:%d%s", schemeName, port, path), true
}

// modelDeploymentDrainDisaggregated reports whether the member runs a disaggregated shape, read
// from the Pod's own rendered command and environment.
//
// THE EVIDENCE IS THE POD'S, NOT THE ROLE'S, and that is what makes this usable where the
// protocol's own check is not. The protocol asks the role, and a role the spec has already dropped
// is nil, so a disaggregated member whose role is gone reaches the read with nothing having refused
// it. The render copies the role's extra arguments into the container command verbatim, and the
// scan below reads the same tokens the protocol's own check reads, so the Pod alone answers the
// same question the role would have.
//
// The scan is deliberately the tree's own, not a second parser. Two parsers of an engine's flags
// would be two answers to which shapes are disaggregated, and they would drift.
func modelDeploymentDrainDisaggregated(engine string, container *corev1.Container) (string, bool) {
	env := make([]workercore.ModelDeploymentEnvVar, 0, len(container.Env))
	for _, entry := range container.Env {
		env = append(env, workercore.ModelDeploymentEnvVar{Name: entry.Name, Value: entry.Value})
	}

	reading, err := scanModelDeploymentParallelism(engine, container.Command, env)
	if err != nil {
		// A shape this tree cannot read is not a shape this read may act on. Refusing it is the
		// direction that cannot delete a busy member.
		return fmt.Sprintf("the member's parallelism could not be read: %v", err), true
	}

	shape, _ := modelDeploymentLoadBalance(reading.declared.Wiring)
	switch shape {
	case workercore.ModelDeploymentLoadBalanceExternal,
		workercore.ModelDeploymentLoadBalanceHybrid,
		workercore.ModelDeploymentLoadBalanceMultiPort:
		return fmt.Sprintf("the member runs a %s disaggregated shape, which has no verifiable "+
			"decoder-side release", shape), true
	}

	if mode := modelDeploymentDrainSGLangDisaggregationMode(container.Command); mode != "" {
		return fmt.Sprintf("the member runs the sglang %s disaggregation mode, which has no "+
			"verifiable decoder-side release", mode), true
	}

	return "", false
}

// modelDeploymentDrainSGLangDisaggregationMode is the SGLang half of the disaggregation question,
// and it is read directly rather than through the tree's balance shapes because those are vLLM's.
//
// The balance table votes a shape only from vLLM's data parallel flags, so an SGLang member launched
// across nodes derives to Internal and the scan above would pass it. SGLang names its own shape on
// the command instead: the server argument takes null, prefill or decode, and a prefill or decode
// member is exactly the one this protocol may not retire on its own gauges. The check is on the
// Pod's own command, which is what makes it survive the role being gone.
func modelDeploymentDrainSGLangDisaggregationMode(command []string) string {
	for i, token := range command {
		value := ""
		switch {
		case token == "--disaggregation-mode" && i+1 < len(command):
			value = command[i+1]
		case strings.HasPrefix(token, "--disaggregation-mode="):
			value = strings.TrimPrefix(token, "--disaggregation-mode=")
		default:
			continue
		}

		if value == "prefill" || value == "decode" {
			return value
		}
	}

	return ""
}
