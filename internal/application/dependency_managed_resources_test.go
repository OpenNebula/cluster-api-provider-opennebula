package application

import (
	"context"
	"strings"
	"testing"

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1beta1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func managedDependencyForTest(t *testing.T) *applicationv1.OneKSApplication {
	t.Helper()
	app := validDependencyPlanApplication(t)
	app.Finalizers = []string{applicationv1.ApplicationFinalizer}
	app.Spec.ManagedResources = []applicationv1.ManagedResourceSpec{managedConfigMap("settings", "monitoring", "settings", nil)}
	refreshOwnedPlan(t, app)
	return app
}

func TestDependencyManagedResourcesAccepted(t *testing.T) {
	assertPlanValid(t, managedDependencyForTest(t))
}

func TestDependencyManagedResourcesGateOwnHelmAndReportReadiness(t *testing.T) {
	ctx := context.Background()
	app := managedDependencyForTest(t)
	app.Spec.ManagedResources[0].Readiness.RequiredResources = []applicationv1.ManagedResourceReference{{
		APIVersion: "v1", Kind: "Secret", Namespace: "monitoring", Name: "prerequisite",
	}}
	refreshOwnedPlan(t, app)
	r, recorder := testReconciler(t, app)
	reader := &metadataSecretReader{Reader: r.Client}
	r.APIReader = reader
	reconcileOnce(t, ctx, r, app)
	assertExists(t, ctx, r.Client, &corev1.ConfigMap{}, "monitoring", "settings")
	assertNotFound(t, ctx, r.Client, helmChartObject(app.Spec.Release.ReleaseName))
	stored := getApplication(t, ctx, r.Client, app)
	condition := meta.FindStatusCondition(stored.Status.Conditions, ConditionResourcesReady)
	if condition == nil || condition.Status != metav1.ConditionFalse || stored.Status.Progress.Total != 2 || len(stored.Status.Resources) != 1 {
		t.Fatalf("dependency resource readiness/progress not reported: %#v", stored.Status)
	}
	reader.secretExists = true
	reconcileOnce(t, ctx, r, app)
	assertExists(t, ctx, r.Client, helmChartObject(app.Spec.Release.ReleaseName), HelmChartNamespace, app.Spec.Release.ReleaseName)
	if got := strings.Join(recorder.childWrites, ","); got != "create:ConfigMap,create:HelmChart" {
		t.Fatalf("unexpected child write order: %s", got)
	}
}
