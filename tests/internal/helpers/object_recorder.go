package helpers

import (
	"context"
	"fmt"
	"sync"
	"time"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
)

// RecorderConfig describes the single object an ObjectRecorder follows. List and Watch must both be
// narrowed to that one object, typically with a metadata.name field selector.
type RecorderConfig[T runtime.Object] struct {
	// Description names the object in errors and logs, e.g. "Node/worker-0".
	Description string
	// List returns the object (zero or one item) and the list's resourceVersion.
	List func(ctx context.Context) ([]T, string, error)
	// Watch opens a watch on the object starting at resourceVersion.
	Watch func(ctx context.Context, resourceVersion string) (watch.Interface, error)
	// RetryInterval is how long to back off after a failed list or watch before trying again.
	RetryInterval time.Duration
	// StartTimeout bounds retries until the initial List and Watch succeed.
	// Zero means a single attempt.
	StartTimeout time.Duration
	// APICallTimeout bounds a List or the establishment of a Watch. The Watch lifetime is separate.
	APICallTimeout time.Duration
	// Logf, if set, receives progress and watch-interruption messages.
	Logf func(format string, args ...any)
}

// RecorderObservation contains one object version and the time that the recorder received it.
type RecorderObservation[T runtime.Object] struct {
	At              time.Time
	Object          T
	UID             types.UID
	ResourceVersion string
	EventType       watch.EventType
}

// RecorderGap identifies an interval with unavailable watch history.
type RecorderGap struct {
	StartedAt time.Time
	EndedAt   time.Time
	Reason    string
}

// RecorderSnapshot contains copied observations and the current watch coverage state.
type RecorderSnapshot[T runtime.Object] struct {
	Observations []RecorderObservation[T]
	Gaps         []RecorderGap
	ReadyAt      time.Time
	PendingSince time.Time
	Watching     bool
	StoppedAt    time.Time
}

// ObjectRecorder keeps, in the background, every version of one object seen by a list+watch, so
// short-lived states can be asserted after the fact.
//
// Polling live state during a disruption races the system under test: a condition, taint or CR can
// appear and disappear between two polls, and a check that starts after the disruption can miss it
// entirely. Start a recorder before the disruption and ask it afterwards what it saw.
//
// The recorder resumes disconnected watches from their last resource version. Expired history
// remains a gap after a relist. Negative assertions must check Coverage as well as observed values.
type ObjectRecorder[T runtime.Object] struct {
	cfg             RecorderConfig[T]
	mu              sync.Mutex
	seen            []T
	exists          bool
	watchErrs       []string
	observations    []RecorderObservation[T]
	gaps            []RecorderGap
	readyAt         time.Time
	pendingSince    time.Time
	watching        bool
	resourceVersion string
	lastProgressAt  time.Time
	stoppedAt       time.Time
	cancel          context.CancelFunc
	wg              sync.WaitGroup
}

// StartObjectRecorder returns after List and Watch succeed. Stop cancels the lifetime Watch.
func StartObjectRecorder[T runtime.Object](cfg RecorderConfig[T]) (*ObjectRecorder[T], error) {
	if cfg.List == nil || cfg.Watch == nil {
		return nil, fmt.Errorf("%s: List and Watch are required", cfg.Description)
	}
	if cfg.APICallTimeout <= 0 {
		cfg.APICallTimeout = 10 * time.Second
	}
	if cfg.RetryInterval <= 0 {
		cfg.RetryInterval = time.Second
	}

	ctx, cancel := context.WithCancel(context.Background())
	rec := &ObjectRecorder[T]{cfg: cfg, cancel: cancel}
	deadline := time.Now().Add(cfg.StartTimeout)
	var resourceVersion string

	for {
		var startErr error
		if resourceVersion == "" {
			resourceVersion, startErr = rec.seed(ctx, rec.startCallTimeout(deadline))
		}

		var watcher watch.Interface
		if startErr == nil {
			watcher, startErr = rec.openWatch(ctx, resourceVersion, rec.startCallTimeout(deadline))
		}
		if startErr == nil {
			rec.mu.Lock()
			rec.readyAt = time.Now()
			rec.lastProgressAt = rec.readyAt
			rec.watching = true
			rec.resourceVersion = resourceVersion
			rec.mu.Unlock()
			rec.wg.Add(1)

			go rec.follow(ctx, watcher)

			rec.logf("Recording %s\n", cfg.Description)

			return rec, nil
		}

		if !time.Now().Before(deadline) {
			cancel()

			return nil, fmt.Errorf("%s: initial List and Watch did not succeed within %s: %w",
				cfg.Description, cfg.StartTimeout, startErr)
		}

		if k8serrors.IsResourceExpired(startErr) || k8serrors.IsGone(startErr) {
			resourceVersion = ""
		}
		retryCtx, stopRetry := context.WithDeadline(ctx, deadline)
		rec.recordWatchErr(retryCtx, startErr)
		stopRetry()
	}
}

func (r *ObjectRecorder[T]) startCallTimeout(deadline time.Time) time.Duration {
	if r.cfg.StartTimeout > 0 {
		return min(r.cfg.APICallTimeout, max(time.Nanosecond, time.Until(deadline)))
	}

	return r.cfg.APICallTimeout
}

// Snapshot returns independent copies of the recorded objects and watch coverage state.
func (r *ObjectRecorder[T]) Snapshot() RecorderSnapshot[T] {
	r.mu.Lock()
	defer r.mu.Unlock()

	snapshot := RecorderSnapshot[T]{
		Gaps:         append([]RecorderGap(nil), r.gaps...),
		ReadyAt:      r.readyAt,
		PendingSince: r.pendingSince,
		Watching:     r.watching,
		StoppedAt:    r.stoppedAt,
		Observations: make([]RecorderObservation[T], len(r.observations)),
	}
	for index, observation := range r.observations {
		snapshot.Observations[index] = observation
		copied, ok := observation.Object.DeepCopyObject().(T)
		if !ok {
			panic("recorded object copy changed its type")
		}
		snapshot.Observations[index].Object = copied
	}

	return snapshot
}

// Coverage rejects intervals with unavailable history or a reconnect that has not reached a bookmark.
func (r *ObjectRecorder[T]) Coverage(start, end time.Time) error {
	snapshot := r.Snapshot()
	if start.Before(snapshot.ReadyAt) || snapshot.ReadyAt.IsZero() {
		return fmt.Errorf("%s: interval starts before Watch readiness", r.cfg.Description)
	}
	if end.Before(start) {
		return fmt.Errorf("%s: interval ends before it starts", r.cfg.Description)
	}
	if end.After(time.Now()) || (!snapshot.StoppedAt.IsZero() && end.After(snapshot.StoppedAt)) {
		return fmt.Errorf("%s: interval ends after observation", r.cfg.Description)
	}
	if !snapshot.PendingSince.IsZero() && !snapshot.PendingSince.After(end) {
		return fmt.Errorf("%s: Watch replay remains pending since %s", r.cfg.Description, snapshot.PendingSince)
	}
	for _, gap := range snapshot.Gaps {
		if !gap.StartedAt.After(end) && !gap.EndedAt.Before(start) {
			return fmt.Errorf("%s: unavailable Watch history: %s", r.cfg.Description, gap.Reason)
		}
	}

	return nil
}

// Stop ends the background watch and waits for it to finish.
func (r *ObjectRecorder[T]) Stop() {
	r.cancel()
	r.wg.Wait()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stoppedAt.IsZero() {
		r.stoppedAt = time.Now()
	}

	r.logf("Stopped recording %s: %d versions observed, %d watch interruptions\n",
		r.cfg.Description, len(r.seen), len(r.watchErrs))
}

// Description returns the configured description of the recorded object.
func (r *ObjectRecorder[T]) Description() string {
	return r.cfg.Description
}

// WatchErrors returns every list or watch interruption seen so far.
func (r *ObjectRecorder[T]) WatchErrors() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]string(nil), r.watchErrs...)
}

// EverExisted returns an Eventually body that is nil once the object has been observed at all.
func (r *ObjectRecorder[T]) EverExisted() func() error {
	return func() error {
		r.mu.Lock()
		defer r.mu.Unlock()

		if len(r.seen) > 0 {
			return nil
		}

		return fmt.Errorf("%s has not been observed yet", r.cfg.Description)
	}
}

// EverSaw returns an Eventually body that is nil once any observed version satisfied pred. It stays
// nil afterwards, even if the object later changes or is deleted. what describes pred in the error.
func (r *ObjectRecorder[T]) EverSaw(what string, pred func(T) bool) func() error {
	return func() error {
		r.mu.Lock()
		defer r.mu.Unlock()

		for _, obj := range r.seen {
			if pred(obj) {
				return nil
			}
		}

		return fmt.Errorf("%s: never observed %s (%d versions seen)", r.cfg.Description, what, len(r.seen))
	}
}

// NeverSaw returns a body for Consistently or a final check: nil as long as no observed version
// satisfied pred. Unlike a live poll, it also catches a version that came and went between polls.
func (r *ObjectRecorder[T]) NeverSaw(what string, pred func(T) bool) func() error {
	return func() error {
		r.mu.Lock()
		defer r.mu.Unlock()

		for idx, obj := range r.seen {
			if pred(obj) {
				return fmt.Errorf("%s: observed %s (version %d of %d)", r.cfg.Description, what, idx+1, len(r.seen))
			}
		}

		return nil
	}
}

// Gone returns an Eventually body that is nil once the object was observed and no longer exists.
func (r *ObjectRecorder[T]) Gone() func() error {
	return func() error {
		r.mu.Lock()
		defer r.mu.Unlock()

		if len(r.seen) == 0 {
			return fmt.Errorf("%s was never observed", r.cfg.Description)
		}

		if r.exists {
			return fmt.Errorf("%s still exists", r.cfg.Description)
		}

		return nil
	}
}

func (r *ObjectRecorder[T]) follow(ctx context.Context, watcher watch.Interface) {
	defer r.wg.Done()
	defer func() {
		r.mu.Lock()
		r.watching = false
		r.mu.Unlock()
	}()

	for ctx.Err() == nil {
		watchErr := r.drain(ctx, watcher)
		watcher.Stop()
		if ctx.Err() != nil {
			return
		}

		r.mu.Lock()
		r.watching = false
		if r.pendingSince.IsZero() {
			r.pendingSince = r.lastProgressAt
		}
		r.mu.Unlock()
		r.recordWatchErr(ctx, watchErr)

		for ctx.Err() == nil {
			if k8serrors.IsResourceExpired(watchErr) || k8serrors.IsGone(watchErr) {
				if seedErr := r.reseedAfterGap(ctx, watchErr); seedErr != nil {
					r.recordWatchErr(ctx, seedErr)

					continue
				}
			}

			r.mu.Lock()
			resourceVersion := r.resourceVersion
			r.mu.Unlock()
			var openErr error
			watcher, openErr = r.openWatch(ctx, resourceVersion, r.cfg.APICallTimeout)
			if openErr != nil {
				watchErr = openErr
				r.recordWatchErr(ctx, openErr)

				continue
			}

			r.mu.Lock()
			r.watching = true
			r.mu.Unlock()

			break
		}
	}
}

func (r *ObjectRecorder[T]) openWatch(
	ctx context.Context, resourceVersion string, timeout time.Duration,
) (watch.Interface, error) {
	type result struct {
		watcher watch.Interface
		err     error
	}

	watchCtx, cancel := context.WithCancel(ctx)
	opened := make(chan result)
	go func() {
		watcher, err := r.cfg.Watch(watchCtx, resourceVersion)
		select {
		case opened <- result{watcher: watcher, err: err}:
		case <-watchCtx.Done():
			if watcher != nil {
				watcher.Stop()
			}
		}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		cancel()

		return nil, ctx.Err()
	case <-timer.C:
		cancel()

		return nil, fmt.Errorf("%s: Watch establishment exceeded %s", r.cfg.Description, timeout)
	case response := <-opened:
		if response.err != nil {
			cancel()

			return nil, response.err
		}
		if response.watcher == nil {
			cancel()

			return nil, fmt.Errorf("%s: Watch returned no stream", r.cfg.Description)
		}

		return &recorderWatch{Interface: response.watcher, cancel: cancel}, nil
	}
}

type recorderWatch struct {
	watch.Interface
	cancel context.CancelFunc
}

func (w *recorderWatch) Stop() {
	w.cancel()
	w.Interface.Stop()
}

func (r *ObjectRecorder[T]) drain(ctx context.Context, watcher watch.Interface) error {
	for {
		var event watch.Event

		select {
		case <-ctx.Done():
			return ctx.Err()
		case received, open := <-watcher.ResultChan():
			if !open {
				return fmt.Errorf("%s: Watch stream closed", r.cfg.Description)
			}

			event = received
		}

		switch event.Type {
		case watch.Added, watch.Modified:
			r.record(event.Object, true, event.Type)
		case watch.Deleted:
			r.record(event.Object, false, event.Type)
		case watch.Error:
			return k8serrors.FromObject(event.Object)
		case watch.Bookmark:
			if accessor, err := meta.Accessor(event.Object); err == nil && accessor.GetResourceVersion() != "" {
				r.mu.Lock()
				r.resourceVersion = accessor.GetResourceVersion()
				r.pendingSince = time.Time{}
				r.lastProgressAt = time.Now()
				r.mu.Unlock()
			}
		default:
		}
	}
}

// seed lists the object, records what the list returned, and returns the list's resourceVersion.
func (r *ObjectRecorder[T]) seed(ctx context.Context, timeout time.Duration) (string, error) {
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	type result struct {
		items           []T
		resourceVersion string
		err             error
	}

	listed := make(chan result, 1)
	go func() {
		items, resourceVersion, err := r.cfg.List(callCtx)
		listed <- result{items: items, resourceVersion: resourceVersion, err: err}
	}()
	var response result
	select {
	case <-callCtx.Done():
		return "", callCtx.Err()
	case response = <-listed:
		if response.err != nil {
			return "", response.err
		}
	}
	if response.resourceVersion == "" {
		return "", fmt.Errorf("%s: List returned an empty resourceVersion", r.cfg.Description)
	}

	for _, item := range response.items {
		r.record(item, true, watch.Added)
	}
	r.mu.Lock()
	r.exists = len(response.items) > 0
	r.resourceVersion = response.resourceVersion
	r.lastProgressAt = time.Now()
	r.mu.Unlock()

	return response.resourceVersion, nil
}

func (r *ObjectRecorder[T]) reseedAfterGap(ctx context.Context, reason error) error {
	r.mu.Lock()
	startedAt := r.pendingSince
	r.mu.Unlock()
	_, err := r.seed(ctx, r.cfg.APICallTimeout)
	if err != nil {
		return err
	}

	r.mu.Lock()
	endedAt := time.Now()
	r.gaps = append(r.gaps, RecorderGap{StartedAt: startedAt, EndedAt: endedAt, Reason: reason.Error()})
	r.pendingSince = endedAt
	r.mu.Unlock()

	return nil
}

// record appends one watched version and whether the object still exists after it.
func (r *ObjectRecorder[T]) record(obj runtime.Object, exists bool, eventType watch.EventType) {
	typed, ok := obj.(T)
	if !ok {
		return
	}
	copied, ok := typed.DeepCopyObject().(T)
	if !ok {
		return
	}
	typed = copied
	accessor, err := meta.Accessor(typed)
	if err != nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.seen = append(r.seen, typed)
	r.exists = exists
	if accessor.GetResourceVersion() != "" {
		r.resourceVersion = accessor.GetResourceVersion()
	}
	observedAt := time.Now()
	r.lastProgressAt = observedAt
	r.observations = append(r.observations, RecorderObservation[T]{
		At: observedAt, Object: typed, UID: accessor.GetUID(),
		ResourceVersion: accessor.GetResourceVersion(), EventType: eventType,
	})
}

// recordWatchErr notes an interruption and backs off before the caller retries. A cancelled context
// is the normal way the recorder ends, so it is neither recorded nor waited out.
func (r *ObjectRecorder[T]) recordWatchErr(ctx context.Context, err error) {
	if ctx.Err() != nil {
		return
	}

	r.mu.Lock()
	r.watchErrs = append(r.watchErrs, err.Error())
	r.mu.Unlock()
	r.logf("Warning: watch on %s interrupted, retrying in %s: %v\n", r.cfg.Description, r.cfg.RetryInterval, err)
	timer := time.NewTimer(r.cfg.RetryInterval)
	defer timer.Stop()

	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

func (r *ObjectRecorder[T]) logf(format string, args ...any) {
	if r.cfg.Logf != nil {
		r.cfg.Logf(format, args...)
	}
}
