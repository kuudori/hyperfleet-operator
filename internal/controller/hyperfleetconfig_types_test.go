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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	hyperfleetv1alpha1 "github.com/openshift-hyperfleet/hyperfleet-operator/api/v1alpha1"
)

// Repeated fixture values are declared once as constants: goconst flags any
// string literal that recurs three or more times in a package, and a single
// definition keeps the specs in sync.
const (
	testTenancyOCID  = "tenancy_ocid"
	testDBSecretName = "hyperfleet-db"
	testIssuerURL    = "https://issuer.example.com"
	testAudience     = "hyperfleet-api"
	// testInvalidSecretName fails SecretReference.Name's DNS-1123 Pattern marker,
	// which is inlined independently at every field that embeds a
	// SecretReference (database, tls).
	testInvalidSecretName = "Invalid_Name"
)

// These specs exercise the CRD's declarative validation and defaulting through a
// real API server (envtest). They assert the schema contract for HYPERFLEET-1406;
// no reconciler behavior is involved.

// validHyperFleetConfig returns a well-formed, minimally-complete singleton that
// the API server must accept. Individual specs mutate a copy to drive negative
// cases.
func validHyperFleetConfig() *hyperfleetv1alpha1.HyperFleetConfig {
	return &hyperfleetv1alpha1.HyperFleetConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: hyperfleetv1alpha1.SingletonName,
		},
		Spec: hyperfleetv1alpha1.HyperFleetConfigSpec{
			Bundle: hyperfleetv1alpha1.BundleCloudCAPI,
			API: hyperfleetv1alpha1.APISpec{
				Database: hyperfleetv1alpha1.DatabaseSpec{
					SecretRef: hyperfleetv1alpha1.SecretReference{Name: testDBSecretName},
				},
			},
			Authentication: hyperfleetv1alpha1.AuthenticationSpec{
				Audience: testAudience,
				IdentityProviders: []hyperfleetv1alpha1.IdentityProvider{{
					Issuer:           testIssuerURL,
					TenantDimensions: map[string]hyperfleetv1alpha1.TenantDimensionSource{testTenancyOCID: {Claim: testTenancyOCID}},
				}},
			},
		},
	}
}

func authWithProviders(providers ...hyperfleetv1alpha1.IdentityProvider) hyperfleetv1alpha1.AuthenticationSpec {
	return hyperfleetv1alpha1.AuthenticationSpec{Audience: testAudience, IdentityProviders: providers}
}

var _ = Describe("HyperFleetConfig CRD validation", func() {
	ctx := context.Background()

	// Every valid object is forced to the same name ("cluster"), so specs collide
	// unless the singleton is torn down between them. Rather than a blanket
	// AfterEach, each spec that actually creates the singleton schedules its own
	// teardown via DeferCleanup(deleteSingletonAndWait, ctx) right after a
	// successful Create: the ~16 negative specs never create, so they register no
	// cleanup and pay nothing. deleteSingletonAndWait lives in suite_test.go and
	// is shared with the reconciler specs.

	Context("singleton enforcement", func() {
		It("accepts the resource when named 'cluster'", func() {
			Expect(k8sClient.Create(ctx, validHyperFleetConfig())).To(Succeed())
			DeferCleanup(deleteSingletonAndWait, ctx)
		})

		It("rejects any name other than 'cluster'", func() {
			obj := validHyperFleetConfig()
			obj.Name = "not-cluster"
			err := k8sClient.Create(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("only permitted name is 'cluster'"))
		})

		It("rejects a second instance as AlreadyExists", func() {
			// The name-pin CEL rule forces every instance to the name "cluster";
			// cluster-scoped name uniqueness then makes the second create collide.
			Expect(k8sClient.Create(ctx, validHyperFleetConfig())).To(Succeed())
			DeferCleanup(deleteSingletonAndWait, ctx)
			err := k8sClient.Create(ctx, validHyperFleetConfig())
			Expect(err).To(HaveOccurred())
			Expect(errors.IsAlreadyExists(err)).To(BeTrue())
		})
	})

	Context("enum validation", func() {
		// Positive coverage: every constant declared in the Go source must be
		// accepted by the generated enum. Generating one spec per value guards
		// against the constants and the +kubebuilder:validation:Enum marker drifting
		// apart.
		for _, bundle := range hyperfleetv1alpha1.AllBundleTypes {
			It("accepts declared bundle value "+string(bundle), func() {
				obj := validHyperFleetConfig()
				obj.Spec.Bundle = bundle
				Expect(k8sClient.Create(ctx, obj)).To(Succeed())
				DeferCleanup(deleteSingletonAndWait, ctx)
			})
		}

		for _, profile := range hyperfleetv1alpha1.AllSizingProfiles {
			It("accepts declared sizing profile "+string(profile), func() {
				obj := validHyperFleetConfig()
				obj.Spec.API.Profile = profile
				Expect(k8sClient.Create(ctx, obj)).To(Succeed())
				DeferCleanup(deleteSingletonAndWait, ctx)
			})
		}

		It("rejects an unknown bundle", func() {
			obj := validHyperFleetConfig()
			obj.Spec.Bundle = "bogus-bundle"
			err := k8sClient.Create(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("Unsupported value"))
		})

		It("rejects an unknown sizing profile", func() {
			obj := validHyperFleetConfig()
			obj.Spec.API.Profile = "huge"
			err := k8sClient.Create(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("Unsupported value"))
		})
	})

	Context("bundle immutability", func() {
		It("rejects changing the bundle after creation", func() {
			obj := validHyperFleetConfig()
			Expect(k8sClient.Create(ctx, obj)).To(Succeed())
			DeferCleanup(deleteSingletonAndWait, ctx)

			obj.Spec.Bundle = hyperfleetv1alpha1.BundleOnPremAgent
			err := k8sClient.Update(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("bundle is immutable"))
		})

		It("allows changing a mutable field (profile) after creation", func() {
			// The immutability rule is scoped to bundle; other fields must remain
			// updatable. This proves self==oldSelf was not applied too broadly.
			obj := validHyperFleetConfig()
			Expect(k8sClient.Create(ctx, obj)).To(Succeed())
			DeferCleanup(deleteSingletonAndWait, ctx)

			obj.Spec.API.Profile = hyperfleetv1alpha1.SizingProfileLarge
			Expect(k8sClient.Update(ctx, obj)).To(Succeed())

			got := &hyperfleetv1alpha1.HyperFleetConfig{}
			Expect(k8sClient.Get(ctx,
				types.NamespacedName{Name: hyperfleetv1alpha1.SingletonName}, got)).To(Succeed())
			Expect(got.Spec.API.Profile).To(Equal(hyperfleetv1alpha1.SizingProfileLarge))
		})
	})

	Context("authentication validation", func() {
		const providerIssuer = "https://idcs-example.identity.oraclecloud.com"
		const secondIssuer = "https://idp.example.org/oauth2"

		createWithProviders := func(providers ...hyperfleetv1alpha1.IdentityProvider) error {
			obj := validHyperFleetConfig()
			obj.Spec.Authentication = authWithProviders(providers...)
			return k8sClient.Create(ctx, obj)
		}

		// expectInvalid asserts the create failed admission validation with an
		// Invalid error, so an unrelated failure (timeout, conflict) cannot
		// masquerade as a schema rejection. Each wantCause substring must appear
		// in the reported status cause.
		expectInvalid := func(err error, wantCause ...string) {
			GinkgoHelper()
			ExpectWithOffset(1, err).To(HaveOccurred())
			ExpectWithOffset(1, errors.IsInvalid(err)).To(BeTrue(), "expected an Invalid admission error, got: %v", err)
			for _, want := range wantCause {
				ExpectWithOffset(1, err.Error()).To(ContainSubstring(want))
			}
		}

		It("accepts several providers with distinct issuers", func() {
			Expect(createWithProviders(
				hyperfleetv1alpha1.IdentityProvider{Issuer: providerIssuer, TenantDimensions: map[string]hyperfleetv1alpha1.TenantDimensionSource{testTenancyOCID: {Claim: testTenancyOCID}}},
				hyperfleetv1alpha1.IdentityProvider{Issuer: secondIssuer, TenantDimensions: map[string]hyperfleetv1alpha1.TenantDimensionSource{"org": {Claim: "org_id"}}},
				hyperfleetv1alpha1.IdentityProvider{Issuer: "https://third.example.net"},
			)).To(Succeed())
			DeferCleanup(deleteSingletonAndWait, ctx)
		})

		It("accepts an issuer with a path", func() {
			Expect(createWithProviders(
				hyperfleetv1alpha1.IdentityProvider{Issuer: "https://idp.example.org/realms/main"},
			)).To(Succeed())
			DeferCleanup(deleteSingletonAndWait, ctx)
		})

		It("accepts per-provider identityClaim", func() {
			Expect(createWithProviders(
				hyperfleetv1alpha1.IdentityProvider{Issuer: providerIssuer, IdentityClaim: "preferred_username", TenantDimensions: map[string]hyperfleetv1alpha1.TenantDimensionSource{testTenancyOCID: {Value: "ocid1.tenancy.oc1..aaaa"}}},
			)).To(Succeed())
			DeferCleanup(deleteSingletonAndWait, ctx)
		})

		It("accepts claim and value tenant dimensions", func() { //nolint:goconst
			Expect(createWithProviders(
				hyperfleetv1alpha1.IdentityProvider{Issuer: providerIssuer, TenantDimensions: map[string]hyperfleetv1alpha1.TenantDimensionSource{
					"tenancy_ocid":   {Claim: "tenancy_ocid"}, //nolint:goconst
					"compartment_id": {Claim: "compartment_id"},
				}},
				hyperfleetv1alpha1.IdentityProvider{Issuer: secondIssuer, TenantDimensions: map[string]hyperfleetv1alpha1.TenantDimensionSource{
					testTenancyOCID: {Value: "ocid1.tenancy.oc1..bbbb"},
				}},
			)).To(Succeed())
			DeferCleanup(deleteSingletonAndWait, ctx)
		})

		It("accepts exactly 64 providers", func() {
			providers := make([]hyperfleetv1alpha1.IdentityProvider, 0, 64)
			for i := range 64 {
				providers = append(providers, hyperfleetv1alpha1.IdentityProvider{Issuer: "https://idp.example.com/" + strings.Repeat("a", 4) + strings.Repeat("b", 4) + "/" + strings.Repeat("x", (i%8)+1) + strings.Repeat("y", i/8+1)})
			}
			Expect(createWithProviders(providers...)).To(Succeed())
			DeferCleanup(deleteSingletonAndWait, ctx)
		})

		It("rejects a config with no authentication block", func() { //nolint:goconst
			u := minimalUnstructured()
			delete(u.Object["spec"].(map[string]any), "authentication")
			err := k8sClient.Create(ctx, u)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("authentication"))
		})

		It("defaults audience to hyperfleet-api when omitted", func() {
			u := minimalUnstructured()
			delete(u.Object["spec"].(map[string]any)["authentication"].(map[string]any), "audience")
			Expect(k8sClient.Create(ctx, u)).To(Succeed())
			DeferCleanup(deleteSingletonAndWait, ctx)
			got := &hyperfleetv1alpha1.HyperFleetConfig{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: hyperfleetv1alpha1.SingletonName}, got)).To(Succeed())
			Expect(got.Spec.Authentication.Audience).To(Equal("hyperfleet-api"))
		})

		It("rejects an explicit empty audience", func() { //nolint:goconst
			u := minimalUnstructured()
			u.Object["spec"].(map[string]any)["authentication"].(map[string]any)["audience"] = ""
			expectInvalid(k8sClient.Create(ctx, u), "should be at least 1 chars long")
		})

		It("rejects an audience longer than 253", func() {
			obj := validHyperFleetConfig()
			obj.Spec.Authentication.Audience = strings.Repeat("a", 254)
			expectInvalid(k8sClient.Create(ctx, obj), "may not be more than 253 bytes")
		})

		It("accepts audience at max length 253", func() {
			obj := validHyperFleetConfig()
			obj.Spec.Authentication.Audience = strings.Repeat("a", 253)
			Expect(k8sClient.Create(ctx, obj)).To(Succeed())
			DeferCleanup(deleteSingletonAndWait, ctx)
		})

		It("accepts claim at max length 256", func() {
			Expect(createWithProviders(
				hyperfleetv1alpha1.IdentityProvider{Issuer: providerIssuer, TenantDimensions: map[string]hyperfleetv1alpha1.TenantDimensionSource{"a": {Claim: strings.Repeat("a", 256)}}},
			)).To(Succeed())
			DeferCleanup(deleteSingletonAndWait, ctx)
		})

		It("accepts value at max length 255", func() {
			Expect(createWithProviders(
				hyperfleetv1alpha1.IdentityProvider{Issuer: providerIssuer, TenantDimensions: map[string]hyperfleetv1alpha1.TenantDimensionSource{"b": {Value: strings.Repeat("b", 255)}}},
			)).To(Succeed())
			DeferCleanup(deleteSingletonAndWait, ctx)
		})

		It("accepts dimension name at max length 63", func() {
			dim := strings.Repeat("a", 63)
			Expect(createWithProviders(
				hyperfleetv1alpha1.IdentityProvider{Issuer: providerIssuer, TenantDimensions: map[string]hyperfleetv1alpha1.TenantDimensionSource{dim: {Claim: "tenancy_ocid"}}},
			)).To(Succeed())
			DeferCleanup(deleteSingletonAndWait, ctx)
		})

		It("rejects dimension name longer than 63", func() {
			dim := strings.Repeat("a", 64)
			expectInvalid(createWithProviders(
				hyperfleetv1alpha1.IdentityProvider{Issuer: providerIssuer, TenantDimensions: map[string]hyperfleetv1alpha1.TenantDimensionSource{dim: {Claim: "tenancy_ocid"}}},
			), "dimension name must match")
		})

		It("rejects more than 8 tenantDimensions", func() {
			dims := make(map[string]hyperfleetv1alpha1.TenantDimensionSource, 9)
			for i := range 9 {
				dims["dim"+strings.Repeat("a", i)] = hyperfleetv1alpha1.TenantDimensionSource{Claim: "tenancy_ocid"}
			}
			expectInvalid(createWithProviders(
				hyperfleetv1alpha1.IdentityProvider{Issuer: providerIssuer, TenantDimensions: dims},
			), "must have at most 8 items")
		})

		It("defaults identityClaim to sub", func() {
			obj := validHyperFleetConfig()
			obj.Spec.Authentication.IdentityProviders[0].IdentityClaim = ""
			Expect(k8sClient.Create(ctx, obj)).To(Succeed())
			DeferCleanup(deleteSingletonAndWait, ctx)
			got := &hyperfleetv1alpha1.HyperFleetConfig{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: hyperfleetv1alpha1.SingletonName}, got)).To(Succeed())
			Expect(got.Spec.Authentication.IdentityProviders[0].IdentityClaim).To(Equal("sub"))
		})

		It("rejects an authentication block with no identityProviders", func() {
			u := minimalUnstructured()
			delete(u.Object["spec"].(map[string]any)["authentication"].(map[string]any), "identityProviders")
			expectInvalid(k8sClient.Create(ctx, u), "identityProviders", "Required value")
		})

		It("rejects an empty identityProviders list", func() {
			obj := validHyperFleetConfig()
			obj.Spec.Authentication.IdentityProviders = []hyperfleetv1alpha1.IdentityProvider{}
			expectInvalid(k8sClient.Create(ctx, obj), "should have at least 1")
		})

		It("rejects more than 64 providers", func() {
			providers := make([]hyperfleetv1alpha1.IdentityProvider, 0, 65)
			for i := range 65 {
				providers = append(providers, hyperfleetv1alpha1.IdentityProvider{Issuer: "https://idp.example.com/p/" + strings.Repeat("a", 5) + "/" + strings.Repeat("x", (i%8)+1) + "/" + strings.Repeat("y", i/8+1)})
			}
			expectInvalid(createWithProviders(providers...), "must have at most 64 items")
		})

		It("rejects a duplicate issuer", func() {
			expectInvalid(createWithProviders(
				hyperfleetv1alpha1.IdentityProvider{Issuer: providerIssuer},
				hyperfleetv1alpha1.IdentityProvider{Issuer: providerIssuer},
			), "Duplicate value")
		})

		It("accepts issuers that differ only by path case", func() {
			Expect(createWithProviders(
				hyperfleetv1alpha1.IdentityProvider{Issuer: "https://idp.example.org/realms/Prod"},
				hyperfleetv1alpha1.IdentityProvider{Issuer: "https://idp.example.org/realms/prod"},
			)).To(Succeed())
			DeferCleanup(deleteSingletonAndWait, ctx)
		})

		It("rejects a non-https issuer", func() {
			expectInvalid(createWithProviders(
				hyperfleetv1alpha1.IdentityProvider{Issuer: "http://idp.example.com"},
			), "issuer must be a valid https URL with a host")
		})

		It("rejects a scheme-only issuer with no host", func() {
			expectInvalid(createWithProviders(
				hyperfleetv1alpha1.IdentityProvider{Issuer: "https://"},
			), "issuer must be a valid https URL with a host")
		})

		It("rejects an empty issuer", func() {
			u := minimalUnstructured()
			auth := u.Object["spec"].(map[string]any)["authentication"].(map[string]any)
			ips := auth["identityProviders"].([]any)
			ips[0].(map[string]any)["issuer"] = ""
			expectInvalid(k8sClient.Create(ctx, u), "should be at least 1 chars long")
		})

		It("rejects an issuer with a query", func() {
			expectInvalid(createWithProviders(
				hyperfleetv1alpha1.IdentityProvider{Issuer: "https://idp.example.com/path?a=b"},
			), "issuer must not contain a query or a fragment")
		})

		It("rejects an issuer with a fragment", func() {
			expectInvalid(createWithProviders(
				hyperfleetv1alpha1.IdentityProvider{Issuer: "https://idp.example.com/path#frag"},
			), "issuer must not contain a query or a fragment")
		})

		It("rejects an issuer with a trailing slash", func() {
			expectInvalid(createWithProviders(
				hyperfleetv1alpha1.IdentityProvider{Issuer: "https://idp.example.com/"},
			), "issuer must not end with a trailing slash")
		})

		It("rejects an issuer longer than 2048", func() {
			obj := validHyperFleetConfig()
			obj.Spec.Authentication.IdentityProviders[0].Issuer = testIssuerURL + "/" + strings.Repeat("a", 2048)
			expectInvalid(k8sClient.Create(ctx, obj), "may not be more than 2048 bytes")
		})

		It("rejects an invalid identityClaim", func() {
			expectInvalid(createWithProviders(
				hyperfleetv1alpha1.IdentityProvider{Issuer: providerIssuer, IdentityClaim: "bad claim!"},
			))
		})

		It("rejects an identityClaim longer than 128", func() {
			expectInvalid(createWithProviders(
				hyperfleetv1alpha1.IdentityProvider{Issuer: providerIssuer, IdentityClaim: strings.Repeat("a", 129)},
			))
		})

		It("rejects a dimension with both claim and value", func() {
			expectInvalid(createWithProviders(
				hyperfleetv1alpha1.IdentityProvider{Issuer: providerIssuer, TenantDimensions: map[string]hyperfleetv1alpha1.TenantDimensionSource{"tenancy_ocid": {Claim: "tenancy_ocid", Value: "ocid1.tenancy.oc1..aaaa"}}},
			), "exactly one of claim or value must be set")
		})

		It("rejects a dimension with neither claim nor value", func() {
			expectInvalid(createWithProviders(
				hyperfleetv1alpha1.IdentityProvider{Issuer: providerIssuer, TenantDimensions: map[string]hyperfleetv1alpha1.TenantDimensionSource{"tenancy_ocid": {}}},
			), "exactly one of claim or value must be set")
		})

		It("rejects an invalid dimension name", func() {
			expectInvalid(createWithProviders(
				hyperfleetv1alpha1.IdentityProvider{Issuer: providerIssuer, TenantDimensions: map[string]hyperfleetv1alpha1.TenantDimensionSource{"bad/name": {Claim: "tenancy_ocid"}}},
			), "dimension name must match")
		})

		It("rejects an invalid claim pattern", func() {
			expectInvalid(createWithProviders(
				hyperfleetv1alpha1.IdentityProvider{Issuer: providerIssuer, TenantDimensions: map[string]hyperfleetv1alpha1.TenantDimensionSource{"tenancy_ocid": {Claim: "bad claim!"}}},
			))
		})

		It("rejects an invalid value pattern", func() {
			expectInvalid(createWithProviders(
				hyperfleetv1alpha1.IdentityProvider{Issuer: providerIssuer, TenantDimensions: map[string]hyperfleetv1alpha1.TenantDimensionSource{testTenancyOCID: {Value: "bad/value!"}}},
			))
		})

		It("rejects a claim longer than 256", func() {
			expectInvalid(createWithProviders(
				hyperfleetv1alpha1.IdentityProvider{Issuer: providerIssuer, TenantDimensions: map[string]hyperfleetv1alpha1.TenantDimensionSource{"tenancy_ocid": {Claim: strings.Repeat("a", 257)}}},
			))
		})

		It("rejects a value longer than 255", func() {
			expectInvalid(createWithProviders(
				hyperfleetv1alpha1.IdentityProvider{Issuer: providerIssuer, TenantDimensions: map[string]hyperfleetv1alpha1.TenantDimensionSource{testTenancyOCID: {Value: strings.Repeat("a", 256)}}},
			))
		})

		It("bumps generation when a provider is added", func() {
			obj := validHyperFleetConfig()
			Expect(k8sClient.Create(ctx, obj)).To(Succeed())
			DeferCleanup(deleteSingletonAndWait, ctx)
			gen0 := obj.Generation
			obj.Spec.Authentication.IdentityProviders = append(obj.Spec.Authentication.IdentityProviders, hyperfleetv1alpha1.IdentityProvider{Issuer: secondIssuer})
			Expect(k8sClient.Update(ctx, obj)).To(Succeed())
			Expect(obj.Generation).To(BeNumerically(">", gen0))
		})
	})

	Context("required fields", func() {
		It("rejects an empty database secret name", func() {
			obj := validHyperFleetConfig()
			obj.Spec.API.Database.SecretRef.Name = ""
			err := k8sClient.Create(ctx, obj)
			Expect(err).To(HaveOccurred())
			// Assert the MinLength message specifically: an empty name also trips the
			// DNS-1123 Pattern, so checking only the field path would keep passing if
			// MinLength=1 were dropped. This isolates the MinLength constraint.
			Expect(err.Error()).To(ContainSubstring("should be at least 1 chars long"))
		})

		It("rejects a database secret name that is not a DNS-1123 subdomain", func() {
			obj := validHyperFleetConfig()
			obj.Spec.API.Database.SecretRef.Name = testInvalidSecretName
			err := k8sClient.Create(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("database.secretRef.name"))
		})

		It("rejects a database secret name longer than the 253-character maximum", func() {
			// A 254-char all-lowercase name satisfies the DNS-1123 Pattern, so only
			// MaxLength can reject it.
			obj := validHyperFleetConfig()
			obj.Spec.API.Database.SecretRef.Name = strings.Repeat("a", 254)
			err := k8sClient.Create(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("may not be more than 253 bytes"))
		})
	})

	Context("optional TLS", func() {
		It("accepts a well-formed TLS secret reference", func() {
			obj := validHyperFleetConfig()
			obj.Spec.API.TLS = &hyperfleetv1alpha1.TLSSpec{
				SecretRef: hyperfleetv1alpha1.SecretReference{Name: "hyperfleet-tls"},
			}
			Expect(k8sClient.Create(ctx, obj)).To(Succeed())
			DeferCleanup(deleteSingletonAndWait, ctx)
		})

		It("rejects a TLS block with an empty secret name", func() {
			obj := validHyperFleetConfig()
			obj.Spec.API.TLS = &hyperfleetv1alpha1.TLSSpec{
				SecretRef: hyperfleetv1alpha1.SecretReference{Name: ""},
			}
			err := k8sClient.Create(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("tls.secretRef.name"))
		})

		It("rejects a TLS secret name that is not a DNS-1123 subdomain", func() {
			// SecretReference.Name's Pattern marker is shared with the database path,
			// but the generated CRD inlines an independent copy at each field; assert
			// it is enforced at the tls.secretRef.name path too, mirroring the
			// database case in the "required fields" context.
			obj := validHyperFleetConfig()
			obj.Spec.API.TLS = &hyperfleetv1alpha1.TLSSpec{
				SecretRef: hyperfleetv1alpha1.SecretReference{Name: testInvalidSecretName},
			}
			err := k8sClient.Create(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("tls.secretRef.name"))
		})

		It("rejects a TLS secret name longer than the 253-character maximum", func() {
			obj := validHyperFleetConfig()
			obj.Spec.API.TLS = &hyperfleetv1alpha1.TLSSpec{
				SecretRef: hyperfleetv1alpha1.SecretReference{Name: strings.Repeat("a", 254)},
			}
			err := k8sClient.Create(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("may not be more than 253 bytes"))
		})
	})

	Context("required object fields", func() {
		// Structural-schema Required markers must reject an object that omits a
		// mandatory field entirely. Only an unstructured object can omit fields the
		// typed client always sends.
		It("rejects a config with no bundle", func() {
			u := minimalUnstructured()
			delete(u.Object["spec"].(map[string]any), "bundle")
			err := k8sClient.Create(ctx, u)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec.bundle"))
		})

		It("rejects a config with no api", func() {
			u := minimalUnstructured()
			delete(u.Object["spec"].(map[string]any), "api")
			err := k8sClient.Create(ctx, u)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec.api"))
		})

		It("rejects an api with no database", func() {
			u := minimalUnstructured()
			api := u.Object["spec"].(map[string]any)["api"].(map[string]any)
			delete(api, "database")
			err := k8sClient.Create(ctx, u)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("database"))
		})

	})

})

// minimalUnstructured builds the smallest valid HyperFleetConfig as an
// unstructured object, omitting every field that carries a server-side default
// (api.profile) so those defaults are exercised.
func minimalUnstructured() *unstructured.Unstructured { //nolint:goconst
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(hyperfleetv1alpha1.GroupVersion.WithKind("HyperFleetConfig"))
	u.SetName(hyperfleetv1alpha1.SingletonName)
	u.Object["spec"] = map[string]any{
		"bundle": string(hyperfleetv1alpha1.BundleCloudCAPI),
		"api": map[string]any{
			"database": map[string]any{
				"secretRef": map[string]any{"name": testDBSecretName},
			},
			// profile omitted -> defaults to "small"
		},
		"authentication": map[string]any{
			"audience":          testAudience,
			"identityProviders": []any{map[string]any{"issuer": testIssuerURL, "tenantDimensions": map[string]any{"tenancy_ocid": map[string]any{"claim": "tenancy_ocid"}}}},
		},
	}
	return u
}
