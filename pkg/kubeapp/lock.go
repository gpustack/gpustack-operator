package kubeapp

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	coordination "k8s.io/api/coordination/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	klog "k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	coordinationv1 "gpustack.ai/gpustack/pkg/kubeclients/kubernetes/typed/coordination/v1"
)

const (
	// defaultLockDuration is how long a claim survives without renewal, i.e. how long a
	// waiter leaves a vanished holder's claim standing before taking it over.
	defaultLockDuration = time.Minute
	// defaultLockTimeout bounds the wait for a claim. Giving up is a failure the caller
	// reports: for a process whose restart is its own retry loop, that is the retry.
	defaultLockTimeout = 5 * time.Minute
	// maxLockRetryInterval caps how long a waiter leaves between reads of the Lease, so
	// that a released claim is picked up promptly however long the claim itself lasts.
	maxLockRetryInterval = 5 * time.Second
)

// errLockTakenOver reports that a peer now holds the claim this process was renewing.
var errLockTakenOver = errors.New("taken over")

// Lock serializes work across processes with a coordination.k8s.io Lease.
//
// It is deliberately not client-go's leaderelection: that package releases its lock when
// the context is canceled, which is only sound if the guarded work has already stopped by
// then. Helm keeps applying past a canceled context, so this lock is released strictly
// after the guarded function has returned — cancellation shortens the work, it does not
// hand the lock to a peer that would then run alongside it.
type Lock struct {
	// Leases is the Lease client of the namespace the lock lives in.
	Leases coordinationv1.LeaseInterface
	// Name is the name of the Lease object.
	Name string
	// Holder identifies this process to its peers. It must be unique per process; two
	// processes sharing one identity would both consider the lock theirs.
	Holder string
	// Duration is how long this process's claim survives without renewal. Zero takes
	// defaultLockDuration.
	Duration time.Duration
	// Timeout bounds the wait for a claim. Zero takes defaultLockTimeout.
	Timeout time.Duration
}

// Do runs fn while holding the lock, and reports fn's error.
//
// It blocks until the Lease is free — never claimed, released, or held by a process that
// stopped renewing — or until the timeout, which it reports as an error without running
// fn. While fn runs, the claim is renewed in the background; it is released once fn
// returns, whether or not fn or the context failed.
func (l Lock) Do(ctx context.Context, fn func(context.Context) error) error {
	if l.Name == "" || l.Holder == "" {
		return fmt.Errorf("lock name and holder are required")
	}

	duration, timeout := l.Duration, l.Timeout
	if duration <= 0 {
		duration = defaultLockDuration
	}
	if timeout <= 0 {
		timeout = defaultLockTimeout
	}

	if err := l.acquire(ctx, duration, timeout); err != nil {
		return err
	}
	klog.InfoS("acquired lock", "lease", l.Name, "holder", l.Holder)

	// The renewal and the release outlive the caller's context on purpose: a canceled
	// context does not stop the work this lock guards, so it must not drop the claim
	// either.
	held := context.WithoutCancel(ctx)

	// Losing the claim cancels the guarded work. It is a wind-down, not a fence: a Helm
	// action already applying keeps going past a canceled context, so this narrows the
	// overlap with whoever took the claim rather than ruling it out.
	guarded, lost := context.WithCancel(ctx)
	defer lost()

	stop, renewed := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(renewed)
		l.renew(held, duration, stop, lost)
	}()

	defer func() {
		// Waiting for the renewal to stop before releasing leaves nothing else writing to
		// the Lease while it is freed.
		close(stop)
		<-renewed
		l.release(held)
	}()

	return fn(guarded)
}

// acquire claims the Lease, waiting out a live holder until the timeout. It reports why
// the last attempt failed, not that the wait ran out: which peer holds the lock is the
// answer whoever reads the failure needs.
func (l Lock) acquire(ctx context.Context, duration, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var seen observation
	for {
		err := l.claim(ctx, duration, &seen)
		if err == nil {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("acquire lock %s as %s: %w", l.Name, l.Holder, err)
		case <-time.After(retryInterval(duration)):
		}
	}
}

// claim makes one attempt at claiming the Lease, recording what it saw of a peer's claim
// so that a later attempt can tell a standing claim from a renewed one.
func (l Lock) claim(ctx context.Context, duration time.Duration, seen *observation) error {
	lease, err := l.Leases.Get(ctx, l.Name, meta.GetOptions{})
	if kerrors.IsNotFound(err) {
		_, err = l.Leases.Create(ctx, l.claimed(nil, duration), meta.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}

	// A claim of this process's own is reclaimed rather than waited out: it is what a
	// previous incarnation under the same identity left behind.
	holder := ptr.Deref(lease.Spec.HolderIdentity, "")
	if holder != "" && holder != l.Holder && !seen.standingFor(lease, duration) {
		return fmt.Errorf("held by %s", holder)
	}

	// The update carries the resource version just read, so of two processes finding the
	// same free Lease exactly one succeeds and the other retries.
	_, err = l.Leases.Update(ctx, l.claimed(lease, duration), meta.UpdateOptions{})

	return err
}

// renew keeps the claim alive until stop is closed, which the holder does once the guarded
// function has returned, or until the claim is gone — whereupon it calls lost, so that the
// work the claim was guarding winds down instead of running beside a peer's.
func (l Lock) renew(ctx context.Context, duration time.Duration, stop <-chan struct{}, lost func()) {
	ticker := time.NewTicker(duration / 3)
	defer ticker.Stop()

	held := time.Now()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}

		err := l.renewOnce(ctx, duration)
		if err == nil {
			held = time.Now()
			continue
		}
		klog.InfoS("renewing lock", "lease", l.Name, "err", err)

		// A claim unrenewed for its whole duration is one a peer may take at any moment,
		// so it counts as lost even before that has been observed.
		if errors.Is(err, errLockTakenOver) || time.Since(held) > duration {
			klog.InfoS("lost lock", "lease", l.Name, "holder", l.Holder, "err", err)
			lost()

			return
		}
	}
}

// renewOnce re-reads the Lease and writes this process's claim over it, reporting
// errLockTakenOver when a peer holds it instead.
func (l Lock) renewOnce(ctx context.Context, duration time.Duration) error {
	lease, err := l.Leases.Get(ctx, l.Name, meta.GetOptions{})
	if err != nil {
		return err
	}
	if holder := ptr.Deref(lease.Spec.HolderIdentity, ""); holder != l.Holder {
		return fmt.Errorf("%w by %s", errLockTakenOver, holder)
	}

	_, err = l.Leases.Update(ctx, l.claimed(lease, duration), meta.UpdateOptions{})

	return err
}

// release frees the Lease, leaving a claim another process has since taken alone.
func (l Lock) release(ctx context.Context) {
	lease, err := l.Leases.Get(ctx, l.Name, meta.GetOptions{})
	if err != nil {
		klog.InfoS("releasing lock", "lease", l.Name, "err", err)
		return
	}
	if ptr.Deref(lease.Spec.HolderIdentity, "") != l.Holder {
		return
	}

	lease.Spec.HolderIdentity = ptr.To("")
	if _, err = l.Leases.Update(ctx, lease, meta.UpdateOptions{}); err != nil {
		// The claim is left to expire, which costs a waiter one Duration.
		klog.InfoS("releasing lock", "lease", l.Name, "err", err)
		return
	}
	klog.InfoS("released lock", "lease", l.Name, "holder", l.Holder)
}

// claimed returns the given Lease claimed by this process, or a new one when there is none.
func (l Lock) claimed(lease *coordination.Lease, duration time.Duration) *coordination.Lease {
	now := meta.NewMicroTime(time.Now())
	if lease == nil {
		lease = &coordination.Lease{ObjectMeta: meta.ObjectMeta{Name: l.Name}}
	}

	if ptr.Deref(lease.Spec.HolderIdentity, "") != l.Holder {
		lease.Spec.AcquireTime = &now
	}
	lease.Spec.HolderIdentity = ptr.To(l.Holder)
	// The field is whole seconds, so the duration is rounded up: truncating it would let a
	// sub-second one claim nothing at all, which reads as expired the moment it is written.
	lease.Spec.LeaseDurationSeconds = ptr.To(int32(math.Ceil(duration.Seconds())))
	lease.Spec.RenewTime = &now

	return lease
}

// observation is a peer's claim as this process last saw it, and when it saw it.
//
// Expiry is judged from how long this process has watched one claim stand unchanged, never
// by measuring its own clock against the renewal timestamp — that timestamp is written by
// another host, and hosts whose clocks differ by more than a claim's duration would each
// read the other's live claim as expired and both proceed.
type observation struct {
	holder    string
	renewedAt meta.MicroTime
	seenAt    time.Time
}

// standingFor reports whether the given claim is the one already being watched and has
// stood unchanged for at least the given duration. A claim seen for the first time never
// qualifies: nothing is yet known about whether its holder is still renewing it.
func (o *observation) standingFor(lease *coordination.Lease, duration time.Duration) bool {
	holder := ptr.Deref(lease.Spec.HolderIdentity, "")
	renewedAt := ptr.Deref(lease.Spec.RenewTime, meta.MicroTime{})

	if o.holder != holder || !o.renewedAt.Equal(&renewedAt) {
		*o = observation{holder: holder, renewedAt: renewedAt, seenAt: time.Now()}
		return false
	}

	return time.Since(o.seenAt) >= duration
}

// retryInterval is how long a waiter leaves between reads of the Lease. It never outruns
// the claim it is watching: a claim has to be read more than once within its own duration
// for standing still to mean anything.
func retryInterval(duration time.Duration) time.Duration {
	if step := duration / 3; step < maxLockRetryInterval {
		return step
	}

	return maxLockRetryInterval
}
