/*
Copyright 2026, OpenNebula Project, OpenNebula Systems.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package application

import (
	"context"
	"errors"
	"testing"

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestRemoveApplicationFinalizerNormally(t *testing.T) {
	ctx := context.Background()
	app := planV1Alpha1FixtureApplication(t)
	app.Finalizers = []string{applicationv1.ApplicationFinalizer}
	reconciler, _ := testReconciler(t, app)
	stored := getApplication(t, ctx, reconciler.Client, app)

	if err := reconciler.removeApplicationFinalizer(ctx, stored); err != nil {
		t.Fatalf("remove application finalizer: %v", err)
	}
	stored = getApplication(t, ctx, reconciler.Client, app)
	if containsString(stored.Finalizers, applicationv1.ApplicationFinalizer) {
		t.Fatalf("application finalizer remains: %#v", stored.Finalizers)
	}
}

func TestRemoveApplicationFinalizerTreatsAuthoritativeNotFoundAsComplete(t *testing.T) {
	ctx := context.Background()
	reconciler, stored, updateErr := finalizerErrorReconciler(t)
	reconciler.APIReader = fake.NewClientBuilder().WithScheme(reconciler.Scheme).Build()

	if err := reconciler.removeApplicationFinalizer(ctx, stored); err != nil {
		t.Fatalf("authoritative NotFound did not complete finalizer removal after %v: %v", updateErr, err)
	}
}

func TestRemoveApplicationFinalizerDoesNotTouchReplacement(t *testing.T) {
	ctx := context.Background()
	reconciler, stored, _ := finalizerErrorReconciler(t)
	replacement := stored.DeepCopy()
	replacement.UID = types.UID("replacement-uid")
	replacement.ResourceVersion = ""
	reconciler.APIReader = fake.NewClientBuilder().WithScheme(reconciler.Scheme).WithObjects(replacement).Build()

	if err := reconciler.removeApplicationFinalizer(ctx, stored); err != nil {
		t.Fatalf("replacement UID did not complete original finalizer removal: %v", err)
	}
	current := &applicationv1.OneKSApplication{}
	if err := reconciler.APIReader.Get(ctx, client.ObjectKeyFromObject(replacement), current); err != nil {
		t.Fatalf("get replacement: %v", err)
	}
	if current.UID != replacement.UID || !containsString(current.Finalizers, applicationv1.ApplicationFinalizer) {
		t.Fatalf("replacement was modified: %#v", current.ObjectMeta)
	}
}

func TestRemoveApplicationFinalizerRetriesWhenSameUIDExists(t *testing.T) {
	ctx := context.Background()
	reconciler, stored, updateErr := finalizerErrorReconciler(t)
	authoritative := stored.DeepCopy()
	authoritative.ResourceVersion = ""
	reconciler.APIReader = fake.NewClientBuilder().WithScheme(reconciler.Scheme).WithObjects(authoritative).Build()

	err := reconciler.removeApplicationFinalizer(ctx, stored)
	if !errors.Is(err, updateErr) {
		t.Fatalf("same UID error = %v, want original update error %v", err, updateErr)
	}
}

func TestRemoveApplicationFinalizerPreservesUpdateErrorWhenAuthoritativeGetFails(t *testing.T) {
	ctx := context.Background()
	reconciler, stored, updateErr := finalizerErrorReconciler(t)
	reconciler.APIReader = &applicationFinalizerGetErrorReader{
		Reader: reconciler.Client,
		err:    errors.New("simulated authoritative read failure"),
	}

	err := reconciler.removeApplicationFinalizer(ctx, stored)
	if err == nil {
		t.Fatal("authoritative read failure incorrectly completed finalizer removal")
	}
	if !errors.Is(err, updateErr) {
		t.Fatalf("authoritative read failure error = %v, want preserved update error %v", err, updateErr)
	}
}

type applicationFinalizerGetErrorReader struct {
	client.Reader
	err error
}

func (r *applicationFinalizerGetErrorReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return r.err
}

type applicationFinalizerUpdateErrorClient struct {
	client.Client
	key client.ObjectKey
	err error
}

func (c *applicationFinalizerUpdateErrorClient) Update(ctx context.Context, object client.Object, options ...client.UpdateOption) error {
	if _, ok := object.(*applicationv1.OneKSApplication); ok && client.ObjectKeyFromObject(object) == c.key {
		return c.err
	}
	return c.Client.Update(ctx, object, options...)
}

func finalizerErrorReconciler(t *testing.T) (*Reconciler, *applicationv1.OneKSApplication, error) {
	t.Helper()
	ctx := context.Background()
	app := planV1Alpha1FixtureApplication(t)
	app.Finalizers = []string{applicationv1.ApplicationFinalizer}
	reconciler, _ := testReconciler(t, app)
	stored := getApplication(t, ctx, reconciler.Client, app)
	updateErr := apierrors.NewConflict(applicationv1.GroupVersion.WithResource("oneksapplications").GroupResource(), stored.Name, errors.New("simulated finalizer update race"))
	reconciler.Client = &applicationFinalizerUpdateErrorClient{
		Client: reconciler.Client, key: client.ObjectKeyFromObject(stored), err: updateErr,
	}
	return reconciler, stored, updateErr
}
