/*
Copyright © The ESO Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package externalsecret

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	esv1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
)

func TestDeletingExternalSecretOnlyDeletesManagedGenericTarget(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := esv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name       string
		storeClass string
		owner      bool
		wantDelete bool
	}{
		{name: "foreign store and owned target", storeClass: "other", owner: true},
		{name: "managed store and unowned target", storeClass: "mine"},
		{name: "managed store and owned target", storeClass: "mine", owner: true, wantDelete: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			const namespace = "team-a"
			deleting := metav1.Now()
			es := &esv1.ExternalSecret{
				Spec: esv1.ExternalSecretSpec{
					SecretStoreRef: esv1.SecretStoreRef{Name: "store", Kind: esv1.SecretStoreKind},
					Data:           []esv1.ExternalSecretData{{RemoteRef: esv1.ExternalSecretDataRemoteRef{Key: "key"}}},
					Target: esv1.ExternalSecretTarget{
						Name: "target", DeletionPolicy: esv1.DeletionPolicyDelete,
						CreationPolicy: esv1.CreatePolicyOwner,
						Manifest:       &esv1.ManifestReference{APIVersion: "v1", Kind: "ConfigMap"},
					},
				},
			}
			es.SetName("example")
			es.SetNamespace(namespace)
			es.SetUID(types.UID("external-secret-uid"))
			es.SetDeletionTimestamp(&deleting)
			es.SetFinalizers([]string{ExternalSecretFinalizer})
			store := &esv1.SecretStore{Spec: esv1.SecretStoreSpec{Controller: tt.storeClass}}
			store.SetName("store")
			store.SetNamespace(namespace)
			target := &v1.ConfigMap{}
			target.SetName("target")
			target.SetNamespace(namespace)
			target.SetUID(types.UID("target-uid"))
			if tt.owner {
				isController := true
				target.OwnerReferences = []metav1.OwnerReference{{
					APIVersion: esv1.SchemeGroupVersion.String(), Kind: "ExternalSecret",
					Name: es.Name, UID: es.UID, Controller: &isController,
				}}
			}
			kube := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(es, store, target).Build()
			r := &Reconciler{
				Client: kube, Scheme: scheme, Log: logr.Discard(), recorder: record.NewFakeRecorder(1),
				ControllerClass: "mine", AllowGenericTargets: true,
			}
			request := ctrl.Request{NamespacedName: types.NamespacedName{Name: es.Name, Namespace: namespace}} //nolint:modernize // embedded field requires an explicit type
			if _, err := r.Reconcile(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			err := kube.Get(context.Background(), client.ObjectKeyFromObject(target), &v1.ConfigMap{})
			if tt.wantDelete {
				if !apierrors.IsNotFound(err) {
					t.Errorf("target still exists or lookup failed: %v", err)
				}
			} else if err != nil {
				t.Errorf("target should remain: %v", err)
			}
		})
	}
}
