package monitor

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func deployment(ready int64) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "api", "namespace": "payments"},
		"status":   map[string]any{"readyReplicas": ready},
	}}
}

func TestPollerReadsEveryConfiguredResource(t *testing.T) {
	second := deployment(2)
	second.SetName("api-two")
	poller := newTestResourcePoller(t, fake.NewSimpleClientset(), deployment(1), second)
	firstSpec := mustConfig(t)[0]
	secondSpec := firstSpec.ResourceSpec
	secondSpec.ID, secondSpec.Name = "deployment-two-ready", "api-two"
	secondQuery, err := prepareResource(secondSpec)
	if err != nil {
		t.Fatal(err)
	}
	for i, spec := range []resourceQuery{firstSpec, secondQuery} {
		if value := pollValue(t, poller, spec); value.Value != int64(i+1) {
			t.Fatalf("resource %s: %#v", spec.ID, value)
		}
	}
}

func TestPollerReportsCurrentValueEveryTime(t *testing.T) {
	poller := newTestResourcePoller(t, fake.NewSimpleClientset(), deployment(1))
	spec := mustConfig(t)[0]
	for _, ready := range []int64{1, 1, 2} {
		if _, err := poller.dynamicClient.Resource(spec.gvr).Namespace(spec.Namespace).Update(context.Background(), deployment(ready), metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		if value := pollValue(t, poller, spec); value.Value != ready || value.Path != spec.Path {
			t.Fatalf("ready replicas %d: %#v", ready, value)
		}
	}
}

func TestPollerEmptyConfigDisablesPolling(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "capone-resource-monitor", Namespace: "kube-system"},
		Data:       map[string]string{ConfigDataKey: "[]"},
	})
	poller := newTestResourcePoller(t, client)
	poller.active = mustConfig(t)
	poller.refreshConfig(context.Background())
	if len(poller.active) != 0 {
		t.Fatal("empty configuration remained active")
	}
}

func TestPollerRetainsValidConfigAndReloadsAfterRecreation(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	poller := newTestResourcePoller(t, client, deployment(1))
	configMaps := client.CoreV1().ConfigMaps("kube-system")
	for _, document := range []string{validConfig, validConfig, "invalid", "", validConfig} {
		if document == "" {
			if err := configMaps.Delete(ctx, "capone-resource-monitor", metav1.DeleteOptions{}); err != nil {
				t.Fatal(err)
			}
		} else {
			configMap := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "capone-resource-monitor", Namespace: "kube-system"},
				Data:       map[string]string{ConfigDataKey: document},
			}
			_, err := configMaps.Update(ctx, configMap, metav1.UpdateOptions{})
			if apierrors.IsNotFound(err) {
				_, err = configMaps.Create(ctx, configMap, metav1.CreateOptions{})
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		poller.refreshConfig(ctx)
		if document == "" {
			if len(poller.active) != 0 {
				t.Fatal("deleted configuration is still active")
			}
			continue
		}
		if len(poller.active) != 1 {
			t.Fatalf("active resources = %d, want 1", len(poller.active))
		}
		if value := pollValue(t, poller, poller.active[0]); value.Value != int64(1) {
			t.Fatalf("polled value: %#v", value)
		}
	}
}

func TestPollerReportsNilForMissingObject(t *testing.T) {
	poller := newTestResourcePoller(t, fake.NewSimpleClientset())
	if value := pollValue(t, poller, mustConfig(t)[0]); value.Value != nil {
		t.Fatalf("missing object: %#v", value)
	}
}

func TestPollerRetainsConfigOnPermissionErrors(t *testing.T) {
	poller := newTestResourcePoller(t, fake.NewSimpleClientset())
	poller.dynamicClient.(*dynamicfake.FakeDynamicClient).PrependReactor("get", "deployments", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "deployments"}, "api", nil)
	})
	poller.active = mustConfig(t)
	if err := poller.pollResource(context.Background(), poller.active[0]); !apierrors.IsForbidden(err) {
		t.Fatalf("permission error: %v", err)
	}
	if len(poller.active) != 1 || len(poller.reports.pending) != 0 {
		t.Fatal("permission error changed configuration or queued a value")
	}
}

func TestPollerRetriesWhenDeliveryQueueRejectsValue(t *testing.T) {
	config := mustConfig(t)[0]
	queue := newReportQueue(&recordingSender{})
	defer queue.queue.ShutDown()
	for i := 0; i < maxPendingReports; i++ {
		queue.Add(fmt.Sprint(i), Report{})
	}
	poller := &resourcePoller{dynamicClient: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), deployment(1)), reports: queue, now: time.Now}
	if err := poller.pollResource(context.Background(), config); err == nil {
		t.Fatal("full queue accepted a new resource")
	}
	queue.processNext(context.Background())
	if err := poller.pollResource(context.Background(), config); err != nil {
		t.Fatalf("retry after freeing capacity: %v", err)
	}
	report, _ := NewResourceValue(config.ResourceSpec, int64(1), time.Now())
	if queue.pending[report.Identity()] == nil {
		t.Fatal("retried value was not queued")
	}
}

func newTestResourcePoller(t *testing.T, client kubernetes.Interface, objects ...runtime.Object) *resourcePoller {
	t.Helper()
	config := Config{}
	if err := config.defaultResourcePolling(); err != nil {
		t.Fatal(err)
	}
	queue := newReportQueue(&recordingSender{})
	t.Cleanup(queue.queue.ShutDown)
	return &resourcePoller{
		dynamicClient: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), objects...),
		configMaps:    client.CoreV1().ConfigMaps(config.ResourceConfigNamespace),
		config:        config, reports: queue, now: time.Now,
	}
}

// Deliver synchronously so polling tests need no goroutines or timeouts.
func pollValue(t *testing.T, poller *resourcePoller, spec resourceQuery) ResourceValue {
	t.Helper()
	if err := poller.pollResource(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if poller.reports.queue.Len() != 1 {
		t.Fatalf("queued reports = %d, want 1", poller.reports.queue.Len())
	}
	poller.reports.processNext(context.Background())
	sender := poller.reports.sender.(*recordingSender)
	value := sender.payloads[len(sender.payloads)-1].(ResourceValue)
	if value.ID != spec.ID {
		t.Fatalf("resource ID = %s, want %s", value.ID, spec.ID)
	}
	return value
}

func mustConfig(t *testing.T) resourceConfig {
	t.Helper()
	config, err := parseResourceConfig([]byte(validConfig))
	if err != nil {
		t.Fatal(err)
	}
	return config
}
