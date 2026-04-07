package gox

import (
	"errors"
	"runtime"
	"sync"

	pond "github.com/alitto/pond/v2"
	klog "k8s.io/klog/v2"
)

var (
	once sync.Once
	gp   = NewPool(100)
)

// NewPool creates a new goroutine pool with the given factor.
//
// The factor is used to calculate the maximum number of workers in the pool,
// which is based on the number of available CPU cores.
//
// The factor should be a positive integer, and if it is less than 10, it will be set to 10 by default.
func NewPool(factor int) pond.Pool {
	// NB(thxCode): Go allows us to create goroutines at will, but if we create too many goroutines,
	// it will cause the program to crash due to insufficient memory,
	// so we need to limit the number of goroutines with pooling.
	//
	// The advantage of pooling is that space is exchanged for time and the reuse rate is improved.
	//
	// - MaxWorkers is the total goroutine number should the pool creates,
	//   we take the number of available CPU cores as the basic value at present,
	//   then times the given factor to get it.
	// - MaxQueueSize is the max value of submitting goroutine number should be accepted at the same time,
	//   we take 150% of the MaxWorkers as the result.
	factor = max(factor, 10)
	maxWorkers := runtime.GOMAXPROCS(0) * factor
	maxQueueSize := maxWorkers / 10 * 15
	return pond.NewPool(maxWorkers, pond.WithQueueSize(maxQueueSize))
}

// Configure sets the goroutine pool with a new factor once,
// multiple calls will be ignored.
func Configure(factor int) {
	once.Do(func() {
		gp.Stop()
		gp = NewPool(factor)
	})
}

// Go submits a task as goroutine.
func Go(f func()) {
	if _, ok := gp.TrySubmit(f); !ok {
		klog.Background().WithName("gopool").
			V(5).
			Info("goroutine pool full")
		gp.Submit(f)
	}
}

// Submit submits an error-returnable task as goroutine.
func Submit(f func() error) pond.Task {
	t, ok := gp.TrySubmitErr(f)
	if !ok {
		klog.Background().WithName("gopool").
			V(5).
			Info("goroutine pool full")
		t = gp.SubmitErr(f)
	}
	return t
}

// IsHealthy returns nil if the pool has plenty workers.
func IsHealthy(atLeast ...int) error {
	watermark := 0
	if len(atLeast) > 0 {
		watermark = max(atLeast[0], 0)
	}
	idle := max(gp.MaxConcurrency()-int(gp.RunningWorkers()), 0)

	if idle > watermark {
		return nil
	}
	return errors.New("goroutine pool full")
}

// Burst returns the maximum number of goroutines to submit at the same moment.
func Burst() int {
	return max(gp.MaxConcurrency()-int(gp.RunningWorkers()), gp.QueueSize(), 0)
}
