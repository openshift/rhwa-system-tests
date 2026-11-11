package tests

import (
	"testing"
	"time"

	"github.com/medik8s/system-tests/tests/internal/helpers"
	"github.com/medik8s/system-tests/tests/sbr-operator/internal/sbrparams"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
)

func TestBPNPodRemovalNeedsEvidenceBeforeDeadline(t *testing.T) {
	start := time.Unix(100, 0)
	deadline := start.Add(300 * time.Second)
	original := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "workload", Namespace: "test", UID: "original"}}
	for _, example := range []struct {
		name  string
		at    time.Time
		uid   types.UID
		event watch.EventType
		want  bool
	}{
		{"timely removal", deadline, original.UID, watch.Deleted, true},
		{"late removal", deadline.Add(time.Second), original.UID, watch.Deleted, false},
		{"old removal", start.Add(-time.Second), original.UID, watch.Deleted, false},
		{"unrelated deletion", deadline, "different", watch.Deleted, false},
		{"timely replacement", deadline, "replacement", watch.Added, true},
		{"late replacement", deadline.Add(time.Second), "replacement", watch.Added, false},
	} {
		t.Run(example.name, func(t *testing.T) {
			pod := original.DeepCopy()
			pod.UID = example.uid
			history := []helpers.RecorderObservation[*corev1.Pod]{{
				At: example.at, UID: example.uid, Object: pod, EventType: example.event,
			}}
			if got := bpnPodGoneBefore(history, original, start, deadline); got != example.want {
				t.Fatalf("removal evidence = %t, want %t", got, example.want)
			}
		})
	}
}

func TestBPNHealthIntervalNeedsHealthyBaselineAndHistory(t *testing.T) {
	start := time.Unix(100, 0)
	makeSample := func(at time.Time, status corev1.ConditionStatus) helpers.RecorderObservation[*corev1.Node] {
		return helpers.RecorderObservation[*corev1.Node]{At: at, UID: "node", Object: &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{UID: "node"},
			Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: "SBRStorageUnhealthy", Status: status}}},
		}}
	}
	for _, example := range []struct {
		name      string
		history   []helpers.RecorderObservation[*corev1.Node]
		wantError bool
	}{
		{"healthy baseline", []helpers.RecorderObservation[*corev1.Node]{makeSample(start, corev1.ConditionFalse)}, false},
		{"missing baseline", []helpers.RecorderObservation[*corev1.Node]{
			makeSample(start.Add(time.Second), corev1.ConditionFalse),
		}, true},
		{"late recovery", []helpers.RecorderObservation[*corev1.Node]{
			makeSample(start, corev1.ConditionTrue), makeSample(start.Add(time.Second), corev1.ConditionFalse),
		}, true},
		{"interrupted recovery", []helpers.RecorderObservation[*corev1.Node]{
			makeSample(start, corev1.ConditionFalse), makeSample(start.Add(time.Second), corev1.ConditionTrue),
			makeSample(start.Add(2*time.Second), corev1.ConditionFalse),
		}, true},
	} {
		t.Run(example.name, func(t *testing.T) {
			err := bpnHealthInterval(example.history, "node", corev1.ConditionFalse, start, start.Add(time.Minute))
			if (err != nil) != example.wantError {
				t.Fatalf("health interval error = %v, want error %t", err, example.wantError)
			}
		})
	}
}

func TestBPNRunningAgentNeedsContainerStatus(t *testing.T) {
	pod := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	if bpnRunningAgent(pod) == nil {
		t.Fatal("a Running Pod without agent status must fail")
	}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: sbrparams.AgentContainerName, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	}}
	if err := bpnRunningAgent(pod); err != nil {
		t.Fatal(err)
	}
}

func TestBPNPeerAgentIntervalIgnoresReporterPods(t *testing.T) {
	start := time.Unix(100, 0)
	agent := bpnPeerAgentFixture()
	baseline := helpers.RecorderObservation[*corev1.Pod]{At: start, UID: agent.UID, Object: agent, EventType: watch.Added}
	reporter := agent.DeepCopy()
	reporter.Name, reporter.UID = "reporter", "reporter"
	reporter.Spec.Containers[0].Name = "reporter"
	reporter.Status.ContainerStatuses = nil
	for _, event := range []watch.EventType{watch.Modified, watch.Deleted} {
		t.Run(string(event), func(t *testing.T) {
			history := []helpers.RecorderObservation[*corev1.Pod]{baseline, {
				At: start.Add(time.Second), UID: reporter.UID, Object: reporter, EventType: event,
			}}
			if err := bpnPeerAgentInterval(history, "test", "peer", start, start.Add(time.Minute)); err != nil {
				t.Fatalf("a reporter event must not fail agent continuity: %v", err)
			}
		})
	}
}

func bpnPeerAgentFixture() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "agent", Namespace: "openshift-workload-availability", UID: "agent",
			Labels: map[string]string{"sbrconfig": "test"},
		},
		Spec: corev1.PodSpec{NodeName: "peer", Containers: []corev1.Container{{Name: sbrparams.AgentContainerName}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
			Name: sbrparams.AgentContainerName, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}}},
	}
}

func TestBPNPeerAgentIntervalRejectsAgentChanges(t *testing.T) {
	start := time.Unix(100, 0)
	for _, change := range []string{"replacement", "deleted", "restart", "stopped"} {
		t.Run(change, func(t *testing.T) {
			agent := bpnPeerAgentFixture()
			changed := agent.DeepCopy()
			event := watch.Modified
			switch change {
			case "replacement":
				changed.UID = "replacement"
			case "deleted":
				event = watch.Deleted
			case "restart":
				changed.Status.ContainerStatuses[0].RestartCount++
			case "stopped":
				changed.Status.ContainerStatuses[0].State.Running = nil
			}
			history := []helpers.RecorderObservation[*corev1.Pod]{
				{At: start, UID: agent.UID, Object: agent, EventType: watch.Added},
				{At: start.Add(time.Second), UID: changed.UID, Object: changed, EventType: event},
			}
			if err := bpnPeerAgentInterval(history, "test", "peer", start, start.Add(time.Minute)); err == nil {
				t.Fatalf("agent %s must fail continuity", change)
			}
		})
	}
}

func TestBPNPeerAgentBaselineUsesPresentPods(t *testing.T) {
	start := time.Unix(100, 0)
	agent := bpnPeerAgentFixture()
	replacement := agent.DeepCopy()
	replacement.Name, replacement.UID = "replacement", "replacement"
	sample := func(pod *corev1.Pod, seconds int, event watch.EventType) helpers.RecorderObservation[*corev1.Pod] {
		return helpers.RecorderObservation[*corev1.Pod]{
			At: start.Add(time.Duration(seconds) * time.Second), UID: pod.UID, Object: pod, EventType: event,
		}
	}
	for _, example := range []struct {
		name      string
		history   []helpers.RecorderObservation[*corev1.Pod]
		wantError bool
	}{
		{"deleted stale Running agent", []helpers.RecorderObservation[*corev1.Pod]{
			sample(agent, -2, watch.Added), sample(agent, -1, watch.Deleted),
		}, true},
		{"replacement before interval", []helpers.RecorderObservation[*corev1.Pod]{
			sample(agent, -3, watch.Added), sample(replacement, -2, watch.Added),
			sample(agent, -1, watch.Deleted), sample(replacement, 1, watch.Modified),
		}, false},
		{"replacement at interval start", []helpers.RecorderObservation[*corev1.Pod]{
			sample(agent, -2, watch.Added), sample(replacement, -1, watch.Added),
			sample(agent, 0, watch.Deleted), sample(replacement, 1, watch.Modified),
		}, false},
		{"two agents present", []helpers.RecorderObservation[*corev1.Pod]{
			sample(agent, -2, watch.Added), sample(replacement, -1, watch.Added),
		}, true},
	} {
		t.Run(example.name, func(t *testing.T) {
			err := bpnPeerAgentInterval(example.history, "test", "peer", start, start.Add(time.Minute))
			if (err != nil) != example.wantError {
				t.Fatalf("baseline error = %v, want error %t", err, example.wantError)
			}
		})
	}
}
