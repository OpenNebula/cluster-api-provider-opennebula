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
	"encoding/json"
	"errors"
	"testing"

	applicationv1 "github.com/OpenNebula/cluster-api-provider-opennebula/api/application/v1alpha5"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestSparseCurrentPlanApplicationFinalizersUseMetadataPatches(t *testing.T) {
	ctx := context.Background()
	app := runAIProtectedPlan(t)
	app.Spec.Dependencies = []applicationv1.DependencyReference{}
	app.Spec.DependencyPlans = []applicationv1.DependencyPlan{}
	app.Spec.ManagedResources = []applicationv1.ManagedResourceSpec{}
	refreshOwnedPlan(t, app)
	wantSpec := app.DeepCopy().Spec
	wantSpecJSON, err := json.Marshal(wantSpec)
	if err != nil {
		t.Fatal(err)
	}
	serialized, err := json.Marshal(app)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(serialized, &wire); err != nil {
		t.Fatal(err)
	}
	wireSpec := wire["spec"].(map[string]any)
	for _, field := range []string{"dependencies", "dependencyPlans", "managedResources"} {
		if _, exists := wireSpec[field]; exists {
			t.Fatalf("empty optional field %s survived typed JSON round-trip: %s", field, serialized)
		}
	}

	reconciler, _ := testReconciler(t, app)
	recorder := &applicationFinalizerMutationClient{
		Client: reconciler.Client, key: client.ObjectKeyFromObject(app),
	}
	reconciler.Client = recorder
	reconcileOnce(t, ctx, reconciler, app)

	stored := getApplication(t, ctx, reconciler.Client, app)
	if !containsString(stored.Finalizers, applicationv1.ApplicationFinalizer) {
		t.Fatalf("application finalizer was not added: %#v", stored.Finalizers)
	}
	assertApplicationSpecJSONUnchanged(t, wantSpecJSON, stored.Spec)
	assertMetadataOnlyApplicationPatches(t, recorder, 1)

	if err := reconciler.removeApplicationFinalizer(ctx, stored); err != nil {
		t.Fatalf("remove application finalizer: %v", err)
	}
	stored = getApplication(t, ctx, reconciler.Client, app)
	if containsString(stored.Finalizers, applicationv1.ApplicationFinalizer) {
		t.Fatalf("application finalizer remains: %#v", stored.Finalizers)
	}
	assertApplicationSpecJSONUnchanged(t, wantSpecJSON, stored.Spec)
	assertMetadataOnlyApplicationPatches(t, recorder, 2)
}

func TestRemoveApplicationFinalizerNormally(t *testing.T) {
	ctx := context.Background()
	app := goldenApplication(t)
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

func TestRemoveApplicationFinalizerHandlesPatchConflictAuthoritatively(t *testing.T) {
	for _, test := range []struct {
		name, authoritative string
		wantPatchError      bool
	}{
		{"not found", "missing", false},
		{"replacement UID", "replacement", false},
		{"same UID", "same", true},
		{"reader failure", "error", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			reconciler, stored, patchErr := finalizerErrorReconciler(t)
			scheme := reconciler.Client.Scheme()
			switch test.authoritative {
			case "missing":
				reconciler.APIReader = fake.NewClientBuilder().WithScheme(scheme).Build()
			case "replacement":
				replacement := stored.DeepCopy()
				replacement.UID = types.UID("replacement-uid")
				replacement.ResourceVersion = ""
				reconciler.APIReader = fake.NewClientBuilder().WithScheme(scheme).WithObjects(replacement).Build()
			case "same":
				authoritative := stored.DeepCopy()
				authoritative.ResourceVersion = ""
				reconciler.APIReader = fake.NewClientBuilder().WithScheme(scheme).WithObjects(authoritative).Build()
			case "error":
				reconciler.APIReader = &applicationFinalizerGetErrorReader{Reader: reconciler.Client, err: errors.New("simulated authoritative read failure")}
			}

			err := reconciler.removeApplicationFinalizer(ctx, stored)
			if test.wantPatchError != errors.Is(err, patchErr) || !test.wantPatchError && err != nil {
				t.Fatalf("remove finalizer error = %v, want original patch error: %t", err, test.wantPatchError)
			}
			if test.authoritative == "replacement" {
				current := &applicationv1.OneKSApplication{}
				if err := reconciler.APIReader.Get(ctx, client.ObjectKeyFromObject(stored), current); err != nil || current.UID != types.UID("replacement-uid") || !containsString(current.Finalizers, applicationv1.ApplicationFinalizer) {
					t.Fatalf("replacement was modified: %#v, %v", current.ObjectMeta, err)
				}
			}
		})
	}
}

type applicationFinalizerGetErrorReader struct {
	client.Reader
	err error
}

func (r *applicationFinalizerGetErrorReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return r.err
}

type applicationFinalizerMutationClient struct {
	client.Client
	key                client.ObjectKey
	patchErr           error
	applicationPatches [][]byte
	applicationUpdates int
}

func (c *applicationFinalizerMutationClient) Patch(ctx context.Context, object client.Object, patch client.Patch, options ...client.PatchOption) error {
	if _, ok := object.(*applicationv1.OneKSApplication); ok && client.ObjectKeyFromObject(object) == c.key {
		payload, err := patch.Data(object)
		if err != nil {
			return err
		}
		c.applicationPatches = append(c.applicationPatches, payload)
		if c.patchErr != nil {
			return c.patchErr
		}
	}
	return c.Client.Patch(ctx, object, patch, options...)
}

func (c *applicationFinalizerMutationClient) Update(ctx context.Context, object client.Object, options ...client.UpdateOption) error {
	if _, ok := object.(*applicationv1.OneKSApplication); ok && client.ObjectKeyFromObject(object) == c.key {
		c.applicationUpdates++
	}
	return c.Client.Update(ctx, object, options...)
}

func finalizerErrorReconciler(t *testing.T) (*Reconciler, *applicationv1.OneKSApplication, error) {
	t.Helper()
	ctx := context.Background()
	app := goldenApplication(t)
	app.Finalizers = []string{applicationv1.ApplicationFinalizer}
	reconciler, _ := testReconciler(t, app)
	stored := getApplication(t, ctx, reconciler.Client, app)
	patchErr := apierrors.NewConflict(applicationv1.GroupVersion.WithResource("oneksapplications").GroupResource(), stored.Name, errors.New("simulated finalizer patch race"))
	reconciler.Client = &applicationFinalizerMutationClient{
		Client: reconciler.Client, key: client.ObjectKeyFromObject(stored), patchErr: patchErr,
	}
	return reconciler, stored, patchErr
}

func assertMetadataOnlyApplicationPatches(t *testing.T, recorder *applicationFinalizerMutationClient, want int) {
	t.Helper()
	if recorder.applicationUpdates != 0 {
		t.Fatalf("application finalizer used %d Update calls", recorder.applicationUpdates)
	}
	if len(recorder.applicationPatches) != want {
		t.Fatalf("application finalizer Patch calls = %d, want %d", len(recorder.applicationPatches), want)
	}
	for _, payload := range recorder.applicationPatches {
		var patch map[string]json.RawMessage
		if err := json.Unmarshal(payload, &patch); err != nil {
			t.Fatalf("decode application finalizer patch %s: %v", payload, err)
		}
		if _, exists := patch["spec"]; exists {
			t.Fatalf("application finalizer patch modified spec: %s", payload)
		}
		var metadata map[string]json.RawMessage
		if err := json.Unmarshal(patch["metadata"], &metadata); err != nil {
			t.Fatalf("decode application finalizer metadata patch %s: %v", payload, err)
		}
		if _, exists := metadata["resourceVersion"]; !exists {
			t.Fatalf("application finalizer patch lacks optimistic resourceVersion: %s", payload)
		}
		if _, exists := metadata["finalizers"]; !exists {
			t.Fatalf("application finalizer patch lacks finalizers: %s", payload)
		}
	}
}

func assertApplicationSpecJSONUnchanged(t *testing.T, want []byte, got applicationv1.OneKSApplicationSpec) {
	t.Helper()
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotJSON) != string(want) {
		t.Fatalf("application spec changed during finalizer mutation:\n got: %s\nwant: %s", gotJSON, want)
	}
}
