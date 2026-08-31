package resourceobserver

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
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
	dynamicClient := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), deployment(1), second)
	values := make(chan ResourceValue, 2)
	poller, err := NewPoller(fake.NewSimpleClientset(), dynamicClient, Options{}, func(_ string, value ResourceValue) bool {
		values <- value
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	secondConfig := strings.ReplaceAll(validConfig, "deployment-ready", "deployment-two-ready")
	secondConfig = strings.Replace(secondConfig, "name: api\n", "name: api-two\n", 1)
	config, err := ParseConfig([]byte(validConfig + secondConfig))
	if err != nil {
		t.Fatal(err)
	}
	poller.active = config
	for _, spec := range config {
		_ = poller.pollResource(context.Background(), spec)
	}
	got := map[string]any{}
	for range 2 {
		value := receiveValue(t, values)
		got[value.ID] = value.Value
	}
	if got["deployment-ready"] != int64(1) || got["deployment-two-ready"] != int64(2) {
		t.Fatalf("unexpected resource values: %#v", got)
	}
}

func TestPollerReportsCurrentValueEveryTime(t *testing.T) {
	dynamicClient := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), deployment(1))
	values := make(chan ResourceValue, 4)
	poller, err := NewPoller(fake.NewSimpleClientset(), dynamicClient, Options{}, func(_ string, value ResourceValue) bool {
		values <- value
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	poller.active = mustConfig(t)
	ctx := context.Background()
	_ = poller.pollResource(ctx, poller.active[0])
	first := receiveValue(t, values)
	if first.Value != int64(1) || first.Path != "status.readyReplicas" {
		t.Fatalf("unexpected initial value: %#v", first)
	}
	_ = poller.pollResource(ctx, poller.active[0])
	if repeated := receiveValue(t, values); repeated.Value != int64(1) {
		t.Fatalf("unchanged value was not reported: %#v", repeated)
	}

	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	object, _ := dynamicClient.Resource(gvr).Namespace("payments").Get(ctx, "api", metav1.GetOptions{})
	_ = unstructured.SetNestedField(object.Object, int64(2), "status", "readyReplicas")
	if _, err := dynamicClient.Resource(gvr).Namespace("payments").Update(ctx, object, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	_ = poller.pollResource(ctx, poller.active[0])
	if updated := receiveValue(t, values); updated.Value != int64(2) {
		t.Fatalf("updated value was not reported: %#v", updated)
	}
}

func TestPollerHotReloadsConfig(t *testing.T) {
	typedClient := fake.NewSimpleClientset()
	dynamicClient := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), deployment(1))
	values := make(chan ResourceValue, 4)
	poller, err := NewPoller(typedClient, dynamicClient, Options{}, func(_ string, value ResourceValue) bool {
		values <- value
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	poller.options.PollInterval = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go poller.Run(ctx)
	configMap, err := typedClient.CoreV1().ConfigMaps("kube-system").Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "capone-resource-monitor", Namespace: "kube-system"},
		Data:       map[string]string{ConfigDataKey: validConfig},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	receiveValue(t, values)

	configMap.Data[ConfigDataKey] = strings.Replace(validConfig, "status.readyReplicas", "status.availableReplicas", 1)
	if _, err := typedClient.CoreV1().ConfigMaps("kube-system").Update(ctx, configMap, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if value := receiveValue(t, values); value.Path != "status.availableReplicas" || value.Value != nil {
		t.Fatalf("updated configuration was not applied: %#v", value)
	}
}

func TestPollerEmptyConfigDisablesPolling(t *testing.T) {
	typedClient := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "capone-resource-monitor", Namespace: "kube-system"},
		Data:       map[string]string{ConfigDataKey: "[]\n"},
	})
	poller, err := NewPoller(typedClient, dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()), Options{}, func(string, ResourceValue) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	poller.active = mustConfig(t)
	poller.refreshConfig(context.Background())
	if len(poller.active) != 0 {
		t.Fatal("empty configuration remained active")
	}
}

func TestPollerReportsNilForMissingObject(t *testing.T) {
	typedClient := fake.NewSimpleClientset()
	dynamicClient := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	values := make(chan ResourceValue, 1)
	poller, err := NewPoller(typedClient, dynamicClient, Options{}, func(_ string, value ResourceValue) bool {
		values <- value
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	poller.active = mustConfig(t)
	_ = poller.pollResource(context.Background(), poller.active[0])
	if value := receiveValue(t, values); value.Value != nil {
		t.Fatalf("missing object did not produce null: %#v", value)
	}
}

func TestPollerRetainsConfigOnPermissionErrors(t *testing.T) {
	dynamicClient := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	dynamicClient.PrependReactor("get", "deployments", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "deployments"}, "api", nil)
	})
	poller, err := NewPoller(fake.NewSimpleClientset(), dynamicClient, Options{}, func(string, ResourceValue) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	poller.active = mustConfig(t)
	_ = poller.pollResource(context.Background(), poller.active[0])
	if poller.active == nil {
		t.Fatal("permission error dropped the active configuration")
	}
}

func TestPollerRetriesWhenDeliveryQueueRejectsValue(t *testing.T) {
	dynamicClient := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), deployment(1))
	attempts := 0
	poller, err := NewPoller(fake.NewSimpleClientset(), dynamicClient, Options{}, func(string, ResourceValue) bool {
		attempts++
		return attempts > 1
	})
	if err != nil {
		t.Fatal(err)
	}
	poller.active = mustConfig(t)
	_ = poller.pollResource(context.Background(), poller.active[0])
	_ = poller.pollResource(context.Background(), poller.active[0])
	if attempts != 2 {
		t.Fatalf("rejected value was not retried: attempts=%d", attempts)
	}
}

func mustConfig(t *testing.T) Config {
	t.Helper()
	config, err := ParseConfig([]byte(validConfig))
	if err != nil {
		t.Fatal(err)
	}
	return config
}

func receiveValue(t *testing.T, values <-chan ResourceValue) ResourceValue {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for resource value")
		return ResourceValue{}
	}
}
