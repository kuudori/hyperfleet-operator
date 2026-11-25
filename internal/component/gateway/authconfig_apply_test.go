/*
Copyright 2026.

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

// Fixture values are spelled out so each case reads like the AuthConfig it applies.
//
//nolint:goconst
package gateway_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	hyperfleetv1alpha1 "github.com/openshift-hyperfleet/hyperfleet-operator/api/v1alpha1"
	"github.com/openshift-hyperfleet/hyperfleet-operator/internal/apply"
	"github.com/openshift-hyperfleet/hyperfleet-operator/internal/component/gateway"
)

// TestAuthConfig_AcceptedByAuthorinoCRD applies the rendered AuthConfig
// through the operator's apply path to a real API server with Authorino's CRD
// installed, which enforces the full schema including CEL rules that the
// in-process strict decode cannot. make test supplies the envtest assets.
func TestAuthConfig_AcceptedByAuthorinoCRD(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("requires envtest assets; run make test")
	}
	g := NewWithT(t)

	// The CRD ships inside the Authorino module the tests already import.
	dir, err := exec.CommandContext(t.Context(), "go", "list", "-m", "-f", "{{.Dir}}", "github.com/kuadrant/authorino").Output()
	g.Expect(err).NotTo(HaveOccurred())
	crd := filepath.Join(strings.TrimSpace(string(dir)), "install", "crd", "authorino.kuadrant.io_authconfigs.yaml")

	env := &envtest.Environment{
		BinaryAssetsDirectory: assets,
		UseExistingCluster:    ptr.To(false),
		CRDInstallOptions:     envtest.CRDInstallOptions{Paths: []string{crd}, ErrorIfPathMissing: true},
	}
	cfg, err := env.Start()
	g.Expect(err).NotTo(HaveOccurred())
	t.Cleanup(func() { g.Expect(env.Stop()).To(Succeed()) })

	s := runtime.NewScheme()
	g.Expect(scheme.AddToScheme(s)).To(Succeed())
	g.Expect(hyperfleetv1alpha1.AddToScheme(s)).To(Succeed())
	c, err := client.New(cfg, client.Options{Scheme: s})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(c.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNamespace}})).To(Succeed())

	// An owner reference needs a UID but not an existing owner: envtest runs no
	// garbage collector.
	owner := &hyperfleetv1alpha1.HyperFleetConfig{ObjectMeta: metav1.ObjectMeta{
		Name: hyperfleetv1alpha1.SingletonName,
		UID:  types.UID("00000000-0000-0000-0000-000000000001"),
	}}

	// Each bundle names its own dimensions; a key from another bundle is rejected.
	providers := map[hyperfleetv1alpha1.BundleType][]hyperfleetv1alpha1.IdentityProvider{
		hyperfleetv1alpha1.BundleCloudCAPI: {
			{Issuer: issuerA, TenantDimensions: map[string]hyperfleetv1alpha1.TenantDimensionSource{"tenancy_ocid": {Value: "ocid-a"}}},
			{Issuer: issuerB, IdentityClaim: "preferred_username", TenantDimensions: map[string]hyperfleetv1alpha1.TenantDimensionSource{
				"tenancy_ocid": {Value: "ocid-b"}, "compartment_id": {Claim: "comp"},
			}},
		},
		hyperfleetv1alpha1.BundleOnPremAgent: {
			{Issuer: issuerA, TenantDimensions: map[string]hyperfleetv1alpha1.TenantDimensionSource{"org": {Value: "acme"}}},
			{Issuer: issuerB, IdentityClaim: "preferred_username", TenantDimensions: map[string]hyperfleetv1alpha1.TenantDimensionSource{
				"org": {Value: "globex"}, "project": {Claim: "proj"},
			}},
		},
	}
	applyTo := func(g *WithT, bundle hyperfleetv1alpha1.BundleType, mutate func(*unstructured.Unstructured) error) (*unstructured.Unstructured, error) {
		obj := render(g, newCR("", providers[bundle]...), testNamespace, modelFor(g, bundle), []string{"hyperfleet-sentinel-clusters"})
		if mutate != nil {
			g.Expect(mutate(obj)).To(Succeed(), "fixture mutation failed")
		}
		return obj, apply.Objects(t.Context(), c, nil, owner, s, []client.Object{obj})
	}

	tests := []struct {
		name    string
		bundle  hyperfleetv1alpha1.BundleType
		mutate  func(*unstructured.Unstructured) error
		wantErr string
	}{
		{name: "cloud-capi AuthConfig is valid", bundle: hyperfleetv1alpha1.BundleCloudCAPI},
		{name: "onprem-agent AuthConfig is valid", bundle: hyperfleetv1alpha1.BundleOnPremAgent},
		{
			// Control cases: prove the API server enforces the schema, so the valid
			// cases above mean something.
			name:   "an operator outside Authorino's enum is rejected",
			bundle: hyperfleetv1alpha1.BundleCloudCAPI,
			mutate: func(obj *unstructured.Unstructured) error {
				bad := []any{map[string]any{"selector": "auth.identity.hf_system", "operator": "nq", "value": "true"}}
				return unstructured.SetNestedSlice(obj.Object, bad, "spec", "authorization", "require-identity", "when")
			},
			wantErr: `Unsupported value: "nq"`,
		},
		{
			// An Authorino CRD without jwt.issuer must fail the apply instead of
			// pruning the field, which would silently stop checking iss.
			name:   "a field unknown to the CRD is rejected, not pruned",
			bundle: hyperfleetv1alpha1.BundleCloudCAPI,
			mutate: func(obj *unstructured.Unstructured) error {
				key := humanBlocks(NewWithT(t), obj)[issuerA]
				return unstructured.SetNestedField(obj.Object, issuerA, "spec", "authentication", key, "jwt", "issuerFromTheFuture")
			},
			wantErr: "field not declared in schema",
		},
		{
			name:   "a string tokenDuration is rejected",
			bundle: hyperfleetv1alpha1.BundleCloudCAPI,
			mutate: func(obj *unstructured.Unstructured) error {
				return unstructured.SetNestedField(obj.Object, "300", "spec", "response", "success", "dynamicMetadata", "wristband", "wristband", "tokenDuration")
			},
			wantErr: "expected numeric",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			t.Cleanup(func() {
				stale := &unstructured.Unstructured{}
				stale.SetGroupVersionKind(schema.GroupVersionKind{Group: "authorino.kuadrant.io", Version: "v1beta3", Kind: "AuthConfig"})
				stale.SetNamespace(testNamespace)
				stale.SetName(gateway.AuthConfigName)
				if err := c.Delete(context.WithoutCancel(t.Context()), stale); err != nil && !errors.IsNotFound(err) {
					g.Expect(err).NotTo(HaveOccurred())
				}
			})

			obj, err := applyTo(g, tt.bundle, tt.mutate)
			if tt.wantErr != "" {
				g.Expect(err).To(HaveOccurred())
				g.Expect(err.Error()).To(ContainSubstring(tt.wantErr))
				return
			}
			g.Expect(err).NotTo(HaveOccurred())

			// A re-apply must be a no-op: anything the API server rewrites (nulls,
			// defaults) would bump resourceVersion every reconcile and wake Owns().
			before := obj.GetResourceVersion()
			again, err := applyTo(g, tt.bundle, nil)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(again.GetResourceVersion()).To(Equal(before))
		})
	}
}
