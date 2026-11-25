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

// Assertions spell out literal Authorino keys and values so failures read like
// the AuthConfig they describe.
//
//nolint:goconst
package gateway_test

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"testing"

	authorinov1beta3 "github.com/kuadrant/authorino/api/v1beta3"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	hyperfleetv1alpha1 "github.com/openshift-hyperfleet/hyperfleet-operator/api/v1alpha1"
	"github.com/openshift-hyperfleet/hyperfleet-operator/internal/component/gateway"
	"github.com/openshift-hyperfleet/hyperfleet-operator/internal/tenant"
)

const (
	testNamespace = "hyperfleet-system"
	issuerA       = "https://a.idp.example.com"
	issuerB       = "https://b.idp.example.com"
	issuerC       = "https://c.idp.example.com"
	issuerD       = "https://d.idp.example.com"
)

func TestAuthConfig(t *testing.T) {
	const saPrefix = "system:serviceaccount:" + testNamespace + ":"
	// The first issuer embeds the 8-character hash suffix that the third one
	// gets when it collides with the second one's block key.
	third := "https://idp.example.com/a/b"
	sum := sha256.Sum256([]byte(third))
	suffixCollision := [3]string{"https://idp.example.com/a-b-" + hex.EncodeToString(sum[:])[:8], "https://idp.example.com/a-b", third}
	tests := []struct {
		name       string
		bundle     hyperfleetv1alpha1.BundleType // default cloud-capi
		namespace  string                        // default testNamespace
		audience   string
		providers  []hyperfleetv1alpha1.IdentityProvider
		machineSAs []string
		wantErr    string // substring; the rendered object must then be nil
		check      func(g *WithT, obj *unstructured.Unstructured)
	}{
		{
			// A CR that skipped CRD defaulting must still get the documented defaults.
			name:      "unset audience and identity claim fall back to the defaults",
			providers: idps(issuerA),
			check: func(g *WithT, obj *unstructured.Unstructured) {
				requireAudience(g, obj, issuerA, hyperfleetv1alpha1.DefaultAuthenticationAudience)
				g.Expect(identityExpression(g, obj, issuerA)).To(Equal(`auth.identity["sub"]`))
			},
		},
		{
			name:      "custom audience and identity claim",
			audience:  "partner-api",
			providers: []hyperfleetv1alpha1.IdentityProvider{{Issuer: issuerA, IdentityClaim: "preferred_username"}},
			check: func(g *WithT, obj *unstructured.Unstructured) {
				requireAudience(g, obj, issuerA, "partner-api")
				g.Expect(identityExpression(g, obj, issuerA)).To(Equal(`auth.identity["preferred_username"]`))
			},
		},
		{
			name:      "machine and human blocks are told apart by Authorization scheme",
			providers: idps(issuerA, issuerB),
			check: func(g *WithT, obj *unstructured.Unstructured) {
				machine := at[map[string]any](g, obj, "spec", "authentication", "hyperfleet-components")
				g.Expect(machine["when"]).To(Equal([]any{map[string]any{"predicate": "request.headers['authorization'].startsWith('ServiceAccount ')"}}))
				g.Expect(machine["overrides"]).To(Equal(map[string]any{"hf_system": map[string]any{"value": "true"}}))
				g.Expect(machine).NotTo(HaveKey("jwt"))

				humans := humanBlocks(g, obj)
				g.Expect(humans).To(HaveLen(2))
				for issuer, key := range humans {
					block := at[map[string]any](g, obj, "spec", "authentication", key)
					g.Expect(block["when"]).To(Equal([]any{map[string]any{"predicate": "request.headers['authorization'].startsWith('Bearer ')"}}), issuer)
					g.Expect(block).NotTo(HaveKey("kubernetesTokenReview"), issuer)
					// Without jwt.issuer Authorino skips the iss check, so a token
					// signed by a provider sharing keys with this one would pass.
					jwt := block["jwt"].(map[string]any)
					g.Expect(jwt).To(HaveKeyWithValue("issuer", issuer))
					g.Expect(jwt).To(HaveKeyWithValue("issuerUrl", issuer))
					overrides := block["overrides"].(map[string]any)
					g.Expect(overrides).To(HaveKeyWithValue("hf_system", map[string]any{"value": "false"}), issuer)
					g.Expect(overrides).To(HaveKey("hf_identity"), issuer)
				}
			},
		},
		{
			name: "a fixed tenant value overrides the token's claim",
			providers: []hyperfleetv1alpha1.IdentityProvider{
				{Issuer: issuerA, TenantDimensions: map[string]hyperfleetv1alpha1.TenantDimensionSource{"tenancy_ocid": {Value: "ocid-a"}}},
			},
			check: func(g *WithT, obj *unstructured.Unstructured) {
				overrides := overridesOf(g, obj, issuerA)
				g.Expect(overrides).To(HaveKeyWithValue("tenancy_ocid", map[string]any{"value": "ocid-a"}))
				g.Expect(overrides).NotTo(HaveKey("compartment_id"))
			},
		},
		{
			name: "a renamed claim is mapped onto the preset claim",
			providers: []hyperfleetv1alpha1.IdentityProvider{
				{Issuer: issuerA, TenantDimensions: map[string]hyperfleetv1alpha1.TenantDimensionSource{
					"tenancy_ocid":   {Value: "ocid-a"},
					"compartment_id": {Claim: "comp"},
				}},
			},
			check: func(g *WithT, obj *unstructured.Unstructured) {
				g.Expect(overridesOf(g, obj, issuerA)).To(HaveKeyWithValue("compartment_id",
					map[string]any{"expression": `"comp" in auth.identity ? auth.identity["comp"] : ''`}))
			},
		},
		{
			// The token's own claim of that name is trusted and reaches the
			// wristband; the CR reference documents this.
			name: "a same-name claim and an unlisted dimension need no override",
			providers: []hyperfleetv1alpha1.IdentityProvider{
				{Issuer: issuerA, TenantDimensions: map[string]hyperfleetv1alpha1.TenantDimensionSource{"tenancy_ocid": {Claim: "tenancy_ocid"}}},
				idp(issuerB),
			},
			check: func(g *WithT, obj *unstructured.Unstructured) {
				for _, issuer := range []string{issuerA, issuerB} {
					g.Expect(overridesOf(g, obj, issuer)).To(HaveLen(2), issuer) // hf_system and hf_identity only
				}
			},
		},
		{
			// Falling back to the preset here would trust the token's tenancy_ocid
			// instead of the fixed value the CR meant to set.
			name: "a misspelled dimension key is rejected",
			providers: []hyperfleetv1alpha1.IdentityProvider{
				{Issuer: issuerA, TenantDimensions: map[string]hyperfleetv1alpha1.TenantDimensionSource{"tenancy-ocid": {Value: "ocid-a"}}},
			},
			wantErr: `unknown tenant dimension "tenancy-ocid"; the bundle defines tenancy_ocid, compartment_id`,
		},
		{
			name:      "cloud-capi requires the tenancy of human callers",
			providers: idps(issuerA),
			check: requireTenantRule(hyperfleetv1alpha1.BundleCloudCAPI,
				[]string{"require-identity", "restrict-system-subjects", "require-tenancy-ocid", "require-string-tenancy-ocid", "require-string-compartment-id"},
				"require-tenancy-ocid", "auth.identity.tenancy_ocid"),
		},
		{
			name:      "onprem-agent requires the org of human callers",
			bundle:    hyperfleetv1alpha1.BundleOnPremAgent,
			providers: idps(issuerA),
			check: requireTenantRule(hyperfleetv1alpha1.BundleOnPremAgent,
				[]string{"require-identity", "restrict-system-subjects", "require-org", "require-string-org", "require-string-project"},
				"require-org", "auth.identity.org_id"),
		},
		{
			// The path is case-sensitive and must be kept as is.
			name:      "issuer scheme and host are lowercased, the path is kept",
			providers: idps("HTTPS://IDP.Example.com:8443/realms/Prod"),
			check: func(g *WithT, obj *unstructured.Unstructured) {
				const want = "https://idp.example.com:8443/realms/Prod"
				key := humanBlocks(g, obj)[want]
				g.Expect(key).NotTo(BeEmpty())
				g.Expect(at[string](g, obj, "spec", "authentication", key, "jwt", "issuerUrl")).To(Equal(want))
			},
		},
		{
			name:      "names, hosts and wristband issuer follow the namespace",
			namespace: "other-ns",
			providers: idps(issuerA),
			check: func(g *WithT, obj *unstructured.Unstructured) {
				g.Expect(obj.GetName()).To(Equal(gateway.AuthConfigName))
				g.Expect(obj.GetNamespace()).To(Equal("other-ns"))
				g.Expect(at[[]any](g, obj, "spec", "hosts")).To(Equal([]any{
					"hyperfleet-gateway.other-ns.svc",
					"hyperfleet-gateway.other-ns.svc.cluster.local",
				}))
				g.Expect(at[string](g, obj, "spec", "response", "success", "dynamicMetadata", "wristband", "wristband", "issuer")).
					To(Equal(gateway.WristbandIssuer("other-ns")))
			},
		},
		{
			name: "issuers that sanitize to the same block key get distinct blocks",
			providers: idps(
				"https://idp.example.com/a-b",
				"https://idp.example.com/a/b",
				"https://idp.example.com/a_b",
			),
			check: requireOwnBlocks("https://idp.example.com/a-b", "https://idp.example.com/a/b", "https://idp.example.com/a_b"),
		},
		{
			name:      "issuers that differ only by path case get distinct blocks",
			providers: idps("https://idp.example.com/realms/Prod", "https://idp.example.com/realms/prod"),
			check:     requireOwnBlocks("https://idp.example.com/realms/Prod", "https://idp.example.com/realms/prod"),
		},
		{
			// The third issuer's suffixed key equals the first issuer's key, which
			// must not be overwritten.
			name:      "a suffixed block key that another issuer already holds is not overwritten",
			providers: idps(suffixCollision[0], suffixCollision[1], suffixCollision[2]),
			check:     requireOwnBlocks(suffixCollision[:]...),
		},
		{
			name:      "an issuer that sanitizes to the machine block key gets its own block",
			providers: idps("https://hyperfleet-components"),
			check:     requireOwnBlocks("https://hyperfleet-components"),
		},
		{
			name:      "the same issuer twice is rejected",
			providers: idps(issuerA, issuerA),
			wantErr:   "same issuer",
		},
		{
			name:      "issuers that differ only by host case are rejected",
			providers: idps("https://IDP.example.com/realms/main", "https://idp.example.com/realms/main"),
			wantErr:   "same issuer",
		},
		{
			name:      "issuers that differ only by scheme case are rejected",
			providers: idps("HTTPS://idp.example.com", "https://idp.example.com"),
			wantErr:   "same issuer",
		},
		{
			name:       "the allowlist admits only the listed ServiceAccounts of the namespace",
			providers:  idps(issuerA),
			machineSAs: []string{"hyperfleet-sentinel-clusters", "hyperfleet-sentinel-nodepools"},
			check: requireAllowlist(
				[]string{saPrefix + "hyperfleet-sentinel-clusters", saPrefix + "hyperfleet-sentinel-nodepools"},
				[]string{
					"system:serviceaccount:other-ns:hyperfleet-sentinel-clusters",
					saPrefix + "hyperfleet-sentinel-clusters-extra",
					saPrefix + "x-hyperfleet-sentinel-clusters",
					saPrefix + "default",
					"x" + saPrefix + "hyperfleet-sentinel-clusters",
				}),
		},
		{
			name:       "regex metacharacters in an allowlisted name are literal",
			providers:  idps(issuerA),
			machineSAs: []string{"sentinel.v1"},
			check:      requireAllowlist([]string{saPrefix + "sentinel.v1"}, []string{saPrefix + "sentinelXv1"}),
		},
		{
			// The allowlist zero value admits no ServiceAccount.
			name:      "an empty allowlist admits no ServiceAccount",
			providers: idps(issuerA),
			check:     requireAllowlist(nil, []string{saPrefix + "hyperfleet-sentinel-clusters", saPrefix + "default"}),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			model := modelFor(g, cmp.Or(tt.bundle, hyperfleetv1alpha1.BundleCloudCAPI))
			namespace := cmp.Or(tt.namespace, testNamespace)

			if tt.wantErr != "" {
				obj, err := gateway.AuthConfig(newCR(tt.audience, tt.providers...), namespace, model, tt.machineSAs)
				g.Expect(err).To(MatchError(ContainSubstring(tt.wantErr)))
				g.Expect(obj).To(BeNil())
				return
			}

			obj := render(g, newCR(tt.audience, tt.providers...), namespace, model, tt.machineSAs)

			// Overrides, never defaults, for every case: a token must not be able
			// to bring its own value for a claim the gateway sets.
			for issuer, key := range humanBlocks(g, obj) {
				g.Expect(at[map[string]any](g, obj, "spec", "authentication", key)).NotTo(HaveKey("defaults"), issuer)
			}
			tt.check(g, obj)
		})
	}
}

func TestAuthConfig_Deterministic(t *testing.T) {
	g := NewWithT(t)
	cr := newCR("partner-api",
		hyperfleetv1alpha1.IdentityProvider{Issuer: issuerA, TenantDimensions: map[string]hyperfleetv1alpha1.TenantDimensionSource{
			"tenancy_ocid":   {Value: "ocid-a"},
			"compartment_id": {Claim: "comp"},
		}},
		idp("https://idp.example.com/a-b"),
		idp("https://idp.example.com/a/b"))
	model := modelFor(g, hyperfleetv1alpha1.BundleCloudCAPI)
	sas := []string{"hyperfleet-sentinel-clusters", "hyperfleet-sentinel-nodepools"}
	crBefore, modelBefore, sasBefore := cr.DeepCopy(), slices.Clone(model), slices.Clone(sas)

	first := render(g, cr, testNamespace, model, sas)
	for range 5 {
		g.Expect(render(g, cr, testNamespace, model, sas).Object).To(Equal(first.Object))
	}

	// The controller hashes these inputs and holds the CR in its cache.
	g.Expect(cr).To(Equal(crBefore))
	g.Expect(model).To(Equal(modelBefore))
	g.Expect(sas).To(Equal(sasBefore))
}

// render also requires the output to decode strictly into Authorino's schema,
// so every case doubles as a schema check.
func render(g *WithT, cr *hyperfleetv1alpha1.HyperFleetConfig, namespace string, model tenant.Model, machineSAs []string) *unstructured.Unstructured {
	obj, err := gateway.AuthConfig(cr, namespace, model, machineSAs)
	g.Expect(err).NotTo(HaveOccurred())
	var decoded authorinov1beta3.AuthConfigSpec
	g.Expect(runtime.DefaultUnstructuredConverter.FromUnstructuredWithValidation(
		at[map[string]any](g, obj, "spec"), &decoded, true)).To(Succeed(), "rendered spec does not match Authorino v1beta3 AuthConfigSpec")
	return obj
}

func at[T any](g Gomega, obj *unstructured.Unstructured, path ...string) T {
	v, found, err := unstructured.NestedFieldNoCopy(obj.Object, path...)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue(), "missing field %v", path)
	typed, ok := v.(T)
	g.Expect(ok).To(BeTrue(), "field %v is %T", path, v)
	return typed
}

func newCR(audience string, providers ...hyperfleetv1alpha1.IdentityProvider) *hyperfleetv1alpha1.HyperFleetConfig {
	cr := &hyperfleetv1alpha1.HyperFleetConfig{}
	cr.Spec.Authentication.Audience = audience
	cr.Spec.Authentication.IdentityProviders = providers
	return cr
}

func idp(issuer string) hyperfleetv1alpha1.IdentityProvider {
	return hyperfleetv1alpha1.IdentityProvider{Issuer: issuer}
}

func idps(issuers ...string) []hyperfleetv1alpha1.IdentityProvider {
	providers := make([]hyperfleetv1alpha1.IdentityProvider, len(issuers))
	for i, issuer := range issuers {
		providers[i] = idp(issuer)
	}
	return providers
}

func modelFor(g *WithT, bundle hyperfleetv1alpha1.BundleType) tenant.Model {
	model, err := tenant.ForBundle(bundle)
	g.Expect(err).NotTo(HaveOccurred())
	return model
}

func humanBlocks(g *WithT, obj *unstructured.Unstructured) map[string]string {
	blocks := map[string]string{}
	for key, block := range at[map[string]any](g, obj, "spec", "authentication") {
		jwt, ok := block.(map[string]any)["jwt"].(map[string]any)
		if !ok {
			continue
		}
		issuer := jwt["issuer"].(string)
		g.Expect(blocks).NotTo(HaveKey(issuer), "issuer rendered twice")
		blocks[issuer] = key
	}
	return blocks
}

// requireAudience checks machine tokens, human tokens and the wristband all use
// the same audience; a mismatch makes one of them rejected downstream.
func requireAudience(g *WithT, obj *unstructured.Unstructured, issuer, want string) {
	human := humanBlocks(g, obj)[issuer]
	g.Expect(at[[]any](g, obj, "spec", "authentication", "hyperfleet-components", "kubernetesTokenReview", "audiences")).To(Equal([]any{want}))
	g.Expect(at[[]any](g, obj, "spec", "authentication", human, "jwt", "audiences")).To(Equal([]any{want}))
	g.Expect(at[string](g, obj, "spec", "response", "success", "dynamicMetadata", "wristband", "wristband", "customClaims", "aud", "value")).To(Equal(want))
}

func identityExpression(g *WithT, obj *unstructured.Unstructured, issuer string) string {
	return at[string](g, obj, "spec", "authentication", humanBlocks(g, obj)[issuer], "overrides", "hf_identity", "expression")
}

func overridesOf(g *WithT, obj *unstructured.Unstructured, issuer string) map[string]any {
	return at[map[string]any](g, obj, "spec", "authentication", humanBlocks(g, obj)[issuer], "overrides")
}

// requireOwnBlocks checks every issuer sits under its own distinct block key,
// next to the machine block.
func requireOwnBlocks(issuers ...string) func(*WithT, *unstructured.Unstructured) {
	return func(g *WithT, obj *unstructured.Unstructured) {
		humans := humanBlocks(g, obj)
		keys := map[string]bool{}
		for _, issuer := range issuers {
			g.Expect(humans).To(HaveKey(issuer))
			keys[humans[issuer]] = true
		}
		g.Expect(keys).To(HaveLen(len(issuers)))
		g.Expect(at[map[string]any](g, obj, "spec", "authentication")).To(HaveLen(len(issuers) + 1))
	}
}

// requireAllowlist checks the machine-subject rule binds to machine callers
// only and admits exactly the allowed usernames.
func requireAllowlist(allow, deny []string) func(*WithT, *unstructured.Unstructured) {
	return func(g *WithT, obj *unstructured.Unstructured) {
		// Without the guard, human usernames would be matched against the pattern too.
		rule := at[map[string]any](g, obj, "spec", "authorization", "restrict-system-subjects")
		g.Expect(rule["when"]).To(Equal([]any{map[string]any{"selector": "auth.identity.hf_system", "operator": "eq", "value": "true"}}))
		patterns := at[[]any](g, obj, "spec", "authorization", "restrict-system-subjects", "patternMatching", "patterns")
		g.Expect(patterns).To(HaveLen(1))
		pattern := patterns[0].(map[string]any)
		g.Expect(pattern).To(HaveKeyWithValue("selector", "auth.identity.user.username"))
		g.Expect(pattern).To(HaveKeyWithValue("operator", "matches"))

		re := regexp.MustCompile(pattern["value"].(string))
		for _, username := range allow {
			g.Expect(re.MatchString(username)).To(BeTrue(), "should allow %q", username)
		}
		for _, username := range deny {
			g.Expect(re.MatchString(username)).To(BeFalse(), "should deny %q", username)
		}
	}
}

// requireTenantRule checks the authorization rules and wristband claims that
// carry the bundle's tenant dimensions.
func requireTenantRule(bundle hyperfleetv1alpha1.BundleType, wantRules []string, requiredRule, requiredSelector string) func(*WithT, *unstructured.Unstructured) {
	return func(g *WithT, obj *unstructured.Unstructured) {
		rules := at[map[string]any](g, obj, "spec", "authorization")
		g.Expect(slices.Collect(maps.Keys(rules))).To(ConsistOf(wantRules))

		g.Expect(rules[requiredRule].(map[string]any)["when"]).
			To(Equal([]any{map[string]any{"selector": "auth.identity.hf_system", "operator": "neq", "value": "true"}}))
		g.Expect(at[[]any](g, obj, "spec", "authorization", requiredRule, "patternMatching", "patterns")).
			To(Equal([]any{map[string]any{"selector": requiredSelector, "operator": "neq", "value": ""}}))

		for _, d := range modelFor(g, bundle) {
			claim := at[string](g, obj, "spec", "response", "success", "dynamicMetadata", "wristband", "wristband", "customClaims", d.Claim, "expression")
			g.Expect(claim).To(ContainSubstring(fmt.Sprintf("%q in auth.identity ?", d.Claim)), d.Key)
		}
		g.Expect(at[map[string]any](g, obj, "spec", "response", "success")).To(HaveLen(1), "tenant values travel as wristband claims, not headers")
	}
}
