package application

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1beta1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// This exercises pruning and CEL admission in a real API server, rather than
// trusting the generated YAML or the fake client's schema-free persistence.
func TestDependencyManagedCRDAdmission(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("set KUBEBUILDER_ASSETS to a Kubernetes envtest bin directory containing kube-apiserver and etcd to exercise CRD admission")
	}
	environment := &envtest.Environment{
		BinaryAssetsDirectory: assets,
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases", "oneks.opennebula.io_oneksapplications.yaml")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := environment.Start()
	if err != nil {
		t.Fatalf("start envtest with generated CRD: %v", err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})
	scheme := runtime.NewScheme()
	if err := applicationv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kube, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := kube.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: applicationv1.ApplicationNamespace}}); err != nil {
		t.Fatal(err)
	}

	plan := managedEmbeddedPlanForTest()
	// Match compiler JSON: these existing schema fields require arrays, not null.
	plan.Dependencies = []applicationv1.DependencyReference{}
	plan.ManagedResources[0].DependsOn = []string{}
	plan.ManagedResources[0].Readiness.Conditions = []applicationv1.ManagedResourceCondition{}
	plan.ManagedResources[0].Readiness.RequiredResources = []applicationv1.ManagedResourceReference{}
	plan.ManagedResources[0].Readiness.Checks = []applicationv1.ManagedResourceCheck{}
	root := validRootPlanGraph(t, []applicationv1.DependencyReference{dependencyReferenceForPlan(plan)}, []applicationv1.DependencyPlan{plan})
	root.UID = ""
	if err := kube.Create(ctx, root); err != nil {
		t.Fatalf("admit root containing managed dependency: %v", err)
	}
	storedRoot := getApplication(t, ctx, kube, root)
	if !reflect.DeepEqual(storedRoot.Spec.DependencyPlans[0].ManagedResources, plan.ManagedResources) {
		t.Fatal("API server pruned embedded managedResources")
	}
	child := expectedDependencyApplication(root, plan)
	if err := kube.Create(ctx, child); err != nil {
		t.Fatalf("admit child with managedResources: %v", err)
	}
	storedChild := getApplication(t, ctx, kube, child)
	if !reflect.DeepEqual(storedChild.Spec.ManagedResources, plan.ManagedResources) {
		t.Fatal("API server pruned child managedResources")
	}

	for _, tc := range []struct {
		name   string
		base   *applicationv1.OneKSApplication
		mutate func(*applicationv1.OneKSApplication)
	}{
		{"child-external-resources", child, func(a *applicationv1.OneKSApplication) {
			a.Spec.ExternalDetection = &applicationv1.ExternalDetectionSpec{Detector: applicationv1.ExternalDetectorCertManager}
		}},
		{"embedded-external-resources", root, func(a *applicationv1.OneKSApplication) {
			a.Spec.DependencyPlans[0].ExternalDetection = &applicationv1.ExternalDetectionSpec{Detector: applicationv1.ExternalDetectorCertManager}
		}},
		{"child-dependency-plans", child, func(a *applicationv1.OneKSApplication) { a.Spec.DependencyPlans = []applicationv1.DependencyPlan{plan} }},
		{"child-protected-secrets", child, func(a *applicationv1.OneKSApplication) {
			protected := runAIProtectedPlan(t)
			a.Spec.ProtectedSecrets = protected.Spec.ProtectedSecrets
			a.Spec.SecretInputRef = protected.Spec.SecretInputRef
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rejected := tc.base.DeepCopy()
			rejected.Name = tc.name
			rejected.UID = ""
			rejected.ResourceVersion = ""
			tc.mutate(rejected)
			err := kube.Create(ctx, rejected)
			if !apierrors.IsInvalid(err) {
				t.Fatalf("expected API validation rejection, got %v", err)
			}
			want := "Root-only dependencyPlans or protected Secret fields"
			if strings.Contains(tc.name, "external-resources") {
				want = "externalDetection must not be combined with managedResources"
			}
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("admission failed for an unrelated reason: %v; want %q", err, want)
			}
		})
	}
}
