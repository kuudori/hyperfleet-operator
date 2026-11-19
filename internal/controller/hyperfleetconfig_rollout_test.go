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

package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	hyperfleetv1alpha1 "github.com/openshift-hyperfleet/hyperfleet-operator/api/v1alpha1"
	"github.com/openshift-hyperfleet/hyperfleet-operator/internal/component"
	apicomponent "github.com/openshift-hyperfleet/hyperfleet-operator/internal/component/api"
)

// Hash-entry ids reused across TestComputeConfigHashProperties and
// TestStampConfigHashSetsAnnotation. One entry per referenced Secret (its
// resourceVersion), not per key — see referencedSecretData.
const (
	dbSecretHashID  = "database"
	tlsSecretHashID = "tls"
)

func TestComputeConfigHashProperties(t *testing.T) {
	g := NewWithT(t)

	base := []hashEntry{
		{id: dbSecretHashID, present: true, value: []byte("1001")},
		{id: tlsSecretHashID, present: true, value: []byte("1002")},
	}

	h := computeConfigHash("config-a", base)
	g.Expect(h).To(HaveLen(64)) // hex-encoded SHA-256

	// Stable and order-independent (entries are sorted by id internally).
	reordered := []hashEntry{base[1], base[0]}
	g.Expect(computeConfigHash("config-a", reordered)).To(Equal(h))

	// Config change → different hash.
	g.Expect(computeConfigHash("config-b", base)).NotTo(Equal(h))

	// resourceVersion change (any write, including rotation) → different hash.
	rotated := []hashEntry{base[0], {id: tlsSecretHashID, present: true, value: []byte("1003")}}
	g.Expect(computeConfigHash("config-a", rotated)).NotTo(Equal(h))

	// Absent vs present-empty must differ (the present/absent discriminator).
	absent := []hashEntry{base[0], {id: tlsSecretHashID, present: false}}
	presentEmpty := []hashEntry{base[0], {id: tlsSecretHashID, present: true, value: []byte("")}}
	g.Expect(computeConfigHash("config-a", absent)).NotTo(Equal(computeConfigHash("config-a", presentEmpty)))

	// A present value equal to the bytes of any absent marker must still differ
	// from a genuinely absent datum: the discriminator byte keeps them distinct so
	// no value can masquerade as "absent".
	presentAbsentLiteral := []hashEntry{base[0], {id: tlsSecretHashID, present: true, value: []byte("<absent>")}}
	g.Expect(computeConfigHash("config-a", absent)).NotTo(Equal(computeConfigHash("config-a", presentAbsentLiteral)))
}

func TestStampConfigHashSetsAnnotation(t *testing.T) {
	g := NewWithT(t)

	cr := &hyperfleetv1alpha1.HyperFleetConfig{
		ObjectMeta: metav1.ObjectMeta{Name: hyperfleetv1alpha1.SingletonName},
		Spec: hyperfleetv1alpha1.HyperFleetConfigSpec{
			Bundle: hyperfleetv1alpha1.BundleCloudCAPI,
			API: hyperfleetv1alpha1.APISpec{
				Database: hyperfleetv1alpha1.DatabaseSpec{
					SecretRef: hyperfleetv1alpha1.SecretReference{Name: testDBSecretName},
				},
			},
			Authentication: hyperfleetv1alpha1.AuthenticationSpec{Audience: testAudience, IdentityProviders: []hyperfleetv1alpha1.IdentityProvider{{Issuer: "https://idcs-example.identity.oraclecloud.com", TenantDimensions: map[string]hyperfleetv1alpha1.TenantDimensionSource{testTenancyOCID: {Claim: testTenancyOCID}}}}},
		},
	}

	render := func() []client.Object {
		objs, err := apicomponent.New("img", "hyperfleet-system", apicomponent.Options{}).Render(context.Background(), cr)
		g.Expect(err).NotTo(HaveOccurred())
		return objs
	}

	apiTarget := component.RolloutTarget{
		ConfigEntries: []component.ConfigEntry{
			{ConfigMapName: apicomponent.ConfigMapName, ConfigFileKey: apicomponent.ConfigFileKey},
		},
		DeploymentName: apicomponent.ResourceName,
	}

	entries := []hashEntry{{id: dbSecretHashID, present: true, value: []byte("h1")}}
	objs := render()
	stampConfigHash(objs, apiTarget, entries)

	depOf := func(objs []client.Object) *appsv1.Deployment {
		for _, o := range objs {
			if d, ok := o.(*appsv1.Deployment); ok {
				return d
			}
		}
		return nil
	}

	dep := depOf(objs)
	g.Expect(dep).NotTo(BeNil())
	got := dep.Spec.Template.Annotations[configHashAnnotation]
	g.Expect(got).NotTo(BeEmpty())

	// Re-stamping the same render with the same entries yields the same hash.
	objs2 := render()
	stampConfigHash(objs2, apiTarget, entries)
	g.Expect(depOf(objs2).Spec.Template.Annotations[configHashAnnotation]).To(Equal(got))

	// A rotated secret value changes the stamped hash.
	objs3 := render()
	stampConfigHash(objs3, apiTarget, []hashEntry{{id: dbSecretHashID, present: true, value: []byte("h2")}})
	g.Expect(depOf(objs3).Spec.Template.Annotations[configHashAnnotation]).NotTo(Equal(got))
}

func TestStampConfigHashMultipleConfigMaps(t *testing.T) {
	g := NewWithT(t)

	const (
		cmA       = "cm-a"
		cmB       = "cm-b"
		cmOther   = "cm-unrelated"
		keyA      = "a.yaml"
		keyB      = "b.yaml"
		targetDep = "target"
		bystander = "bystander"
	)

	target := component.RolloutTarget{
		ConfigEntries: []component.ConfigEntry{
			{ConfigMapName: cmA, ConfigFileKey: keyA},
			{ConfigMapName: cmB, ConfigFileKey: keyB},
		},
		DeploymentName: targetDep,
	}

	// build renders a fresh object set: the two ConfigMaps the target hashes, an
	// unrelated ConfigMap it does not, the target Deployment, and a bystander
	// Deployment that must never be stamped.
	build := func(dataA, dataB, dataOther string) []client.Object {
		return []client.Object{
			&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: cmA}, Data: map[string]string{keyA: dataA}},
			&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: cmB}, Data: map[string]string{keyB: dataB}},
			&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: cmOther}, Data: map[string]string{"x": dataOther}},
			&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: targetDep}},
			&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: bystander}},
		}
	}
	annotationOn := func(objs []client.Object, name string) (string, bool) {
		for _, o := range objs {
			if d, ok := o.(*appsv1.Deployment); ok && d.Name == name {
				v, ok := d.Spec.Template.Annotations[configHashAnnotation]
				return v, ok
			}
		}
		return "", false
	}
	stamp := func(dataA, dataB, dataOther string) string {
		objs := build(dataA, dataB, dataOther)
		returned := stampConfigHash(objs, target, nil)
		stamped, ok := annotationOn(objs, targetDep)
		g.Expect(ok).To(BeTrue(), "the named Deployment must be annotated")
		g.Expect(stamped).To(Equal(returned), "the annotation must carry the returned hash")
		return stamped
	}

	base := stamp("a1", "b1", "o1")
	g.Expect(base).To(HaveLen(64))

	// The annotation lands only on the named Deployment.
	objs := build("a1", "b1", "o1")
	stampConfigHash(objs, target, nil)
	_, ok := annotationOn(objs, bystander)
	g.Expect(ok).To(BeFalse(), "a Deployment that is not the target must not be stamped")

	// Re-stamping identical inputs is stable.
	g.Expect(stamp("a1", "b1", "o1")).To(Equal(base))

	// A change in either ConfigMap's data changes the hash.
	g.Expect(stamp("a2", "b1", "o1")).NotTo(Equal(base), "first ConfigMap changed")
	g.Expect(stamp("a1", "b2", "o1")).NotTo(Equal(base), "second ConfigMap changed")

	// Moving content between the two ConfigMaps changes the hash.
	g.Expect(stamp("b1", "a1", "o1")).NotTo(Equal(base))

	// A ConfigMap outside the target's entries does not affect the hash.
	g.Expect(stamp("a1", "b1", "o2")).To(Equal(base))
}

func TestMapSecretToConfig(t *testing.T) {
	g := NewWithT(t)

	r := &HyperFleetConfigReconciler{OperatorNamespace: operatorNamespace}

	inNS := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Name: "hyperfleet-db", Namespace: operatorNamespace}}
	reqs := r.mapSecretToConfig(context.Background(), inNS)
	g.Expect(reqs).To(HaveLen(1))
	g.Expect(reqs[0].Name).To(Equal(hyperfleetv1alpha1.SingletonName))

	other := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Name: "hyperfleet-db", Namespace: "elsewhere"}}
	g.Expect(r.mapSecretToConfig(context.Background(), other)).To(BeEmpty())
}
