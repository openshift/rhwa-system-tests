package helpers

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
)

// fakeNodeAPI serves a scripted List and hands out one FakeWatcher per Watch call.
type fakeNodeAPI struct {
	mu            sync.Mutex
	listed        []*corev1.Node
	listErr       error
	listCalls     int
	watchVersions []string
	// failLists makes the next failLists List calls fail with listErr.
	failLists int
	watchers  chan *watch.FakeWatcher
}

func newFakeNodeAPI(initial ...*corev1.Node) *fakeNodeAPI {
	return &fakeNodeAPI{listed: initial, watchers: make(chan *watch.FakeWatcher, 10)}
}

func (f *fakeNodeAPI) config() RecorderConfig[*corev1.Node] {
	return RecorderConfig[*corev1.Node]{
		Description: "Node/worker-0",
		List: func(context.Context) ([]*corev1.Node, string, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.listCalls++

			if f.failLists > 0 {
				f.failLists--

				return nil, "", f.listErr
			}

			return f.listed, "1", nil
		},
		Watch: func(_ context.Context, resourceVersion string) (watch.Interface, error) {
			f.mu.Lock()
			f.watchVersions = append(f.watchVersions, resourceVersion)
			f.mu.Unlock()
			fakeWatcher := watch.NewFake()
			f.watchers <- fakeWatcher

			return fakeWatcher, nil
		},
		RetryInterval: time.Millisecond,
	}
}

func (f *fakeNodeAPI) setListed(nodes ...*corev1.Node) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.listed = nodes
}

func (f *fakeNodeAPI) nextWatcher(t *testing.T) *watch.FakeWatcher {
	t.Helper()

	select {
	case fakeWatcher := <-f.watchers:
		return fakeWatcher
	case <-time.After(5 * time.Second):
		t.Fatal("recorder did not open a watch")

		return nil
	}
}

func testNode(tainted bool) *corev1.Node {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-0", UID: "node-uid", ResourceVersion: "1"}}
	if tainted {
		node.Spec.Taints = []corev1.Taint{{Key: corev1.TaintNodeOutOfService}}
	}

	return node
}

func isTainted(node *corev1.Node) bool {
	return len(node.Spec.Taints) > 0
}

// waitFor polls check until it returns nil, failing the test after a few seconds.
func waitFor(t *testing.T, check func() error) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := check()
		if err == nil {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("condition not met: %v", err)
		}

		time.Sleep(time.Millisecond)
	}
}

func TestObjectRecorderKeepsShortLivedState(t *testing.T) {
	api := newFakeNodeAPI(testNode(false))
	rec, err := StartObjectRecorder(api.config())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer rec.Stop()

	if err := rec.EverSaw("taint", isTainted)(); err == nil {
		t.Fatal("EverSaw passed before the taint was ever applied")
	}

	// The taint appears and is removed again before anyone checks: a live read would miss it.
	fakeWatcher := api.nextWatcher(t)
	fakeWatcher.Modify(testNode(true))
	fakeWatcher.Modify(testNode(false))
	waitFor(t, rec.EverSaw("taint", isTainted))

	if err := rec.NeverSaw("taint", isTainted)(); err == nil {
		t.Fatal("NeverSaw passed although the taint was observed")
	}
}

func TestObjectRecorderGone(t *testing.T) {
	api := newFakeNodeAPI()
	rec, err := StartObjectRecorder(api.config())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer rec.Stop()

	if err := rec.Gone()(); err == nil {
		t.Fatal("Gone passed for an object that was never observed")
	}

	fakeWatcher := api.nextWatcher(t)
	fakeWatcher.Add(testNode(false))
	waitFor(t, rec.EverExisted())

	if err := rec.Gone()(); err == nil {
		t.Fatal("Gone passed while the object still exists")
	}

	fakeWatcher.Delete(testNode(false))
	waitFor(t, rec.Gone())
}

func TestObjectRecorderResumesLastVersionAfterWatchCloses(t *testing.T) {
	api := newFakeNodeAPI(testNode(false))
	rec, err := StartObjectRecorder(api.config())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer rec.Stop()

	firstWatcher := api.nextWatcher(t)
	node := testNode(true)
	node.ResourceVersion = "7"
	firstWatcher.Modify(node)
	waitFor(t, rec.EverSaw("taint", isTainted))
	firstWatcher.Stop()
	resumedWatcher := api.nextWatcher(t)
	api.mu.Lock()
	listCalls := api.listCalls
	versions := append([]string(nil), api.watchVersions...)
	api.mu.Unlock()
	if listCalls != 1 || len(versions) != 2 || versions[1] != "7" {
		t.Fatalf("reconnect lost history: lists=%d versions=%v", listCalls, versions)
	}
	if rec.Coverage(rec.Snapshot().ReadyAt, time.Now()) == nil {
		t.Fatal("pending replay must prevent a complete negative interval")
	}

	resumedWatcher.Action(watch.Bookmark, &metav1.PartialObjectMetadata{
		ObjectMeta: metav1.ObjectMeta{ResourceVersion: "8"},
	})
	waitFor(t, func() error { return rec.Coverage(rec.Snapshot().ReadyAt, time.Now()) })
}

func TestObjectRecorderRecordsWatchErrorEvents(t *testing.T) {
	api := newFakeNodeAPI(testNode(false))
	rec, err := StartObjectRecorder(api.config())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer rec.Stop()

	api.nextWatcher(t).Error(&metav1.Status{Message: "too old resource version"})
	api.nextWatcher(t)

	if errs := rec.WatchErrors(); len(errs) != 1 {
		t.Fatalf("expected one recorded watch error, got %v", errs)
	}
}

func TestStartObjectRecorderRetriesInitialList(t *testing.T) {
	api := newFakeNodeAPI(testNode(false))
	api.listErr = errors.New("connection refused")
	api.failLists = 2
	cfg := api.config()
	cfg.StartTimeout = 5 * time.Second
	rec, err := StartObjectRecorder(cfg)
	if err != nil {
		t.Fatalf("expected StartObjectRecorder to retry past transient list failures: %v", err)
	}
	defer rec.Stop()

	if errs := rec.WatchErrors(); len(errs) != 2 {
		t.Fatalf("expected the two failed lists to be recorded, got %v", errs)
	}
}

func TestStartObjectRecorderGivesUpAfterStartTimeout(t *testing.T) {
	api := newFakeNodeAPI()
	api.listErr = errors.New("connection refused")
	api.failLists = 1000
	cfg := api.config()
	cfg.StartTimeout = 20 * time.Millisecond

	if _, err := StartObjectRecorder(cfg); err == nil {
		t.Fatal("expected StartObjectRecorder to fail when no list succeeds within StartTimeout")
	}
}

func TestObjectRecorderWaitsForWatchReadiness(t *testing.T) {
	api := newFakeNodeAPI()
	cfg := api.config()
	opening := make(chan struct{})
	release := make(chan struct{})
	watcher := watch.NewFake()
	cfg.Watch = func(ctx context.Context, _ string) (watch.Interface, error) {
		close(opening)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return watcher, nil
		}
	}
	type result struct {
		recorder *ObjectRecorder[*corev1.Node]
		err      error
	}
	started := make(chan result, 1)
	go func() {
		recorder, err := StartObjectRecorder(cfg)
		started <- result{recorder: recorder, err: err}
	}()
	<-opening
	select {
	case <-started:
		t.Fatal("recorder returned before Watch establishment")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	response := <-started
	if response.err != nil {
		t.Fatal(response.err)
	}
	defer response.recorder.Stop()
	if response.recorder.Snapshot().ReadyAt.IsZero() {
		t.Fatal("recorder did not retain readiness time")
	}
}

func TestObjectRecorderBoundsWatchOpeningWithoutBoundingLifetime(t *testing.T) {
	t.Run("list", func(t *testing.T) {
		api := newFakeNodeAPI()
		cfg := api.config()
		cfg.APICallTimeout = 15 * time.Millisecond
		release := make(chan struct{})
		defer close(release)
		cfg.List = func(context.Context) ([]*corev1.Node, string, error) {
			<-release

			return nil, "1", nil
		}
		if _, err := StartObjectRecorder(cfg); err == nil {
			t.Fatal("unresponsive List must return an error")
		}
	})
	t.Run("opening", func(t *testing.T) {
		api := newFakeNodeAPI()
		cfg := api.config()
		cfg.APICallTimeout = 15 * time.Millisecond
		release := make(chan struct{})
		defer close(release)
		cfg.Watch = func(context.Context, string) (watch.Interface, error) {
			<-release

			return watch.NewFake(), nil
		}
		if _, err := StartObjectRecorder(cfg); err == nil {
			t.Fatal("unresponsive Watch establishment must return an error")
		}
	})
	t.Run("lifetime", func(t *testing.T) {
		api := newFakeNodeAPI(testNode(false))
		cfg := api.config()
		cfg.APICallTimeout = 10 * time.Millisecond
		cfg.Watch = func(ctx context.Context, _ string) (watch.Interface, error) {
			fakeWatcher := watch.NewFake()
			api.watchers <- fakeWatcher
			go func() {
				<-ctx.Done()
				fakeWatcher.Stop()
			}()

			return fakeWatcher, nil
		}
		rec, err := StartObjectRecorder(cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer rec.Stop()
		fakeWatcher := api.nextWatcher(t)
		time.Sleep(30 * time.Millisecond)
		fakeWatcher.Modify(testNode(true))
		waitFor(t, rec.EverSaw("taint", isTainted))
	})
}

func TestObjectRecorderExpiredHistoryRemainsIncomplete(t *testing.T) {
	api := newFakeNodeAPI(testNode(false))
	rec, err := StartObjectRecorder(api.config())
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Stop()
	start := rec.Snapshot().ReadyAt
	api.setListed(testNode(true))
	api.nextWatcher(t).Error(&metav1.Status{
		Reason: metav1.StatusReasonExpired, Code: 410, Message: "expired history",
	})
	resumedWatcher := api.nextWatcher(t)
	waitFor(t, rec.EverSaw("taint", isTainted))
	if rec.Coverage(start, time.Now()) == nil {
		t.Fatal("a relist must not restore missing negative evidence")
	}
	if len(rec.Snapshot().Gaps) != 1 {
		t.Fatal("expired history must retain one gap")
	}

	afterGap := rec.Snapshot().Gaps[0].EndedAt.Add(time.Nanosecond)
	if rec.Coverage(afterGap, time.Now()) == nil {
		t.Fatal("the relisted Watch must establish replay progress before a new negative interval")
	}
	resumedWatcher.Action(watch.Bookmark, &metav1.PartialObjectMetadata{
		ObjectMeta: metav1.ObjectMeta{ResourceVersion: "2"},
	})
	waitFor(t, func() error { return rec.Coverage(afterGap, time.Now()) })
}

func TestObjectRecorderSnapshotsCopyObjectsAndRetainUIDs(t *testing.T) {
	api := newFakeNodeAPI(testNode(false))
	rec, err := StartObjectRecorder(api.config())
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Stop()
	firstSnapshot := rec.Snapshot()
	firstSnapshot.Observations[0].Object.UID = "replacement"
	if err := rec.EverSaw("original UID", func(node *corev1.Node) bool { return node.UID == "node-uid" })(); err != nil {
		t.Fatal(err)
	}
	fakeWatcher := api.nextWatcher(t)
	replacement := testNode(false)
	replacement.UID = "replacement"
	fakeWatcher.Modify(replacement)
	waitFor(t, func() error {
		snapshot := rec.Snapshot()
		if len(snapshot.Observations) < 2 {
			return fmt.Errorf("replacement has not arrived")
		}
		if snapshot.Observations[0].UID != "node-uid" || snapshot.Observations[1].UID != "replacement" {
			t.Fatal("snapshot combined different object identities")
		}

		return nil
	})
}
