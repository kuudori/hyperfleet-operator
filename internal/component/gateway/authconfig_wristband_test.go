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

//nolint:goconst // Literal claim names keep each case readable.
package gateway_test

import (
	"encoding/json"
	"maps"
	"slices"
	"testing"

	authorinocel "github.com/kuadrant/authorino/pkg/expressions/cel"
	"github.com/kuadrant/authorino/pkg/jsonexp"
	. "github.com/onsi/gomega"

	hyperfleetv1alpha1 "github.com/openshift-hyperfleet/hyperfleet-operator/api/v1alpha1"
)

// TestAuthConfig_DimensionClaims evaluates the rendered tenant rules and
// wristband claim expressions with Authorino's own CEL and pattern code, for the
// identity a human caller ends up with once the overrides are applied. Strict
// decoding only proves their shape; this proves what they do with strings,
// arrays, numbers and absent claims. That a token cannot forge these claims
// depends on Authorino applying overrides, which belongs to the end-to-end suite.
func TestAuthConfig_DimensionClaims(t *testing.T) {
	tests := []struct {
		name       string
		claims     map[string]any // dimension claims of the identity, plus hf_identity when it is not "alice"
		wantDenied string         // rule that denies the request; no wristband is issued then
		wantClaims map[string]any // dimension claims of the wristband
	}{
		{
			name:       "string claims reach the wristband",
			claims:     map[string]any{"tenancy_ocid": "t1", "compartment_id": "c1"},
			wantClaims: map[string]any{"tenancy_ocid": "t1", "compartment_id": "c1"},
		},
		{
			name:       "an absent optional claim becomes empty",
			claims:     map[string]any{"tenancy_ocid": "t1"},
			wantClaims: map[string]any{"tenancy_ocid": "t1", "compartment_id": ""},
		},
		{
			name:       "an absent required claim is denied",
			claims:     map[string]any{"compartment_id": "c1"},
			wantDenied: "require-tenancy-ocid",
		},
		{
			name:       "an array-valued required claim is denied",
			claims:     map[string]any{"tenancy_ocid": []any{"t1", "t2"}},
			wantDenied: "require-string-tenancy-ocid",
		},
		{
			// A groups claim mapped onto a dimension has no single value to pick.
			name:       "an array-valued optional claim is denied",
			claims:     map[string]any{"tenancy_ocid": "t1", "compartment_id": []any{"dev", "ops"}},
			wantDenied: "require-string-compartment-id",
		},
		{
			name:       "an array-valued identity claim is denied",
			claims:     map[string]any{"tenancy_ocid": "t1", "hf_identity": []any{"alice", "bob"}},
			wantDenied: "require-identity",
		},
		{
			name:       "a numeric identity claim is denied",
			claims:     map[string]any{"tenancy_ocid": "t1", "hf_identity": 42},
			wantDenied: "require-identity",
		},
		{
			name:       "a numeric claim is denied",
			claims:     map[string]any{"tenancy_ocid": 5},
			wantDenied: "require-string-tenancy-ocid",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			model := modelFor(g, hyperfleetv1alpha1.BundleCloudCAPI)
			obj := render(g, newCR("", idp(issuerA)), testNamespace, model, nil)

			identity := maps.Clone(tt.claims)
			identity["hf_system"] = "false" // always set by the gateway for human callers
			if _, ok := identity["hf_identity"]; !ok {
				identity["hf_identity"] = "alice"
			}
			auth := authJSON(g, identity)

			rules := at[map[string]any](g, obj, "spec", "authorization")
			for _, name := range slices.Sorted(maps.Keys(rules)) {
				rule := rules[name].(map[string]any)
				if !matchAll(g, rule["when"].([]any), auth) {
					continue
				}
				if !matchAll(g, rule["patternMatching"].(map[string]any)["patterns"].([]any), auth) {
					g.Expect(tt.wantDenied).To(Equal(name), "unexpectedly denied by %s", name)
					return
				}
			}
			g.Expect(tt.wantDenied).To(BeEmpty(), "request was not denied")

			wristband := at[map[string]any](g, obj, "spec", "response", "success", "dynamicMetadata", "wristband", "wristband", "customClaims")
			for name, want := range tt.wantClaims {
				expression, err := authorinocel.NewExpression(wristband[name].(map[string]any)["expression"].(string))
				g.Expect(err).NotTo(HaveOccurred(), name)
				got, err := expression.ResolveFor(auth)
				g.Expect(err).NotTo(HaveOccurred(), name)
				g.Expect(got).To(Equal(want), name)
			}
		})
	}
}

func authJSON(g *WithT, identity map[string]any) string {
	raw, err := json.Marshal(map[string]any{"auth": map[string]any{"identity": identity}})
	g.Expect(err).NotTo(HaveOccurred())
	return string(raw)
}

// matchAll reports whether every condition holds; selector patterns and CEL
// predicates are both evaluated by Authorino's own code.
func matchAll(g *WithT, conditions []any, authJSON string) bool {
	for _, c := range conditions {
		condition := c.(map[string]any)
		var (
			matched bool
			err     error
		)
		if predicate, ok := condition["predicate"].(string); ok {
			var p *authorinocel.Predicate
			p, err = authorinocel.NewPredicate(predicate)
			g.Expect(err).NotTo(HaveOccurred(), predicate)
			matched, err = p.Matches(authJSON)
		} else {
			pattern := jsonexp.Pattern{
				Selector: condition["selector"].(string),
				Operator: jsonexp.OperatorFromString(condition["operator"].(string)),
				Value:    condition["value"].(string),
			}
			matched, err = pattern.Matches(authJSON)
		}
		g.Expect(err).NotTo(HaveOccurred())
		if !matched {
			return false
		}
	}
	return true
}
