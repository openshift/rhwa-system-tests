package helpers

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestPrepareNodeAccessRejectsReplacementUID(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-0", UID: "replacement"}}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()
	if _, err := PrepareNodeAccess(context.Background(), k8sClient, node.Name, "original", time.Second); err == nil {
		t.Fatal("prepared access must reject a replacement Node")
	}
}

func TestPreparedNodeAccessUsesCachedArguments(t *testing.T) {
	prepared := &PreparedNodeAccess{
		NodeName: "worker-0", NodeUID: "original", Address: "192.0.2.10",
		sshArgs: []string{"-i", "/prepared/key", "core@192.0.2.10"},
		serial:  make(chan struct{}, 1),
	}
	prepared.command = func(_ context.Context, address string, _ time.Duration, args []string) (string, error) {
		expected := []string{"-i", "/prepared/key", "core@192.0.2.10", "cat /proc/sys/kernel/random/boot_id"}
		if address != prepared.Address || !reflect.DeepEqual(args, expected) {
			t.Fatalf("command changed prepared SSH settings: address=%s args=%v", address, args)
		}

		return "host-boot-id", nil
	}
	bootID, err := prepared.BootID(context.Background(), time.Second)
	if err != nil || bootID != "host-boot-id" {
		t.Fatalf("host boot read failed without an API client: id=%q err=%v", bootID, err)
	}
	if len(prepared.sshArgs) != 3 {
		t.Fatal("command modified cached SSH arguments")
	}
}

func TestPreparedNodeAccessSerializesAndBoundsQueuedCommands(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	prepared := &PreparedNodeAccess{
		NodeName: "worker-0", Address: "192.0.2.10", serial: make(chan struct{}, 1),
	}
	prepared.command = func(ctx context.Context, _ string, _ time.Duration, _ []string) (string, error) {
		close(started)
		select {
		case <-release:
			return "done", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	finished := make(chan error, 1)
	go func() {
		_, err := prepared.Run(context.Background(), time.Second, "first")
		finished <- err
	}()
	<-started
	_, err := prepared.Run(context.Background(), 20*time.Millisecond, "second")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued command must stop at its deadline: %v", err)
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := prepared.Run(context.Background(), time.Second, "after close"); err == nil {
		t.Fatal("closed prepared access accepted a command")
	}
}
