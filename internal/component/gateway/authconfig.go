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

// Package gateway renders the HyperFleet gateway's Authorino AuthConfig. The
// builders are pure functions and never touch the cluster.
//
// Authorino keys and values are written as literals so each builder reads like
// the YAML it renders; goconst would otherwise force constants for them.
//
//nolint:goconst
package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	hyperfleetv1alpha1 "github.com/openshift-hyperfleet/hyperfleet-operator/api/v1alpha1"
	"github.com/openshift-hyperfleet/hyperfleet-operator/internal/component"
	"github.com/openshift-hyperfleet/hyperfleet-operator/internal/tenant"
)

const gatewayServiceName = "hyperfleet-gateway"
const AuthorinoName = "authorino"

const (
	AuthConfigName                = "hyperfleet-auth"
	WristbandSigningKeySecretName = "hyperfleet-wristband-signing-key"

	claimHFSystem   = "hf_system"
	claimHFIdentity = "hf_identity"
	selHFSystem     = "auth.identity." + claimHFSystem
	selHFIdentity   = "auth.identity." + claimHFIdentity
	systemTrue      = "true"
)

// WristbandIssuer is the issuer URL the API validates wristbands against.
func WristbandIssuer(namespace string) string {
	return fmt.Sprintf("https://%s-authorino-oidc.%s.svc:%d/%s/%s/wristband",
		AuthorinoName, namespace, 8083, namespace, AuthConfigName)
}

// Render an AuthConfig with which Authorino authenticates machine
// and human callers, authorizes them, and exchanges the result for a wristband
// carrying the tenant claims, which is all the API trusts. It fails when two
// identity providers have the same issuer once the host is lowercased.
func AuthConfig(cr *hyperfleetv1alpha1.HyperFleetConfig, namespace string, model tenant.Model, machineSAs []string) (*unstructured.Unstructured, error) {
	authn := cr.Spec.Authentication
	audience := authn.Audience
	if audience == "" {
		audience = hyperfleetv1alpha1.DefaultAuthenticationAudience
	}

	authBlocks, err := authentication(audience, authn.IdentityProviders, model)
	if err != nil {
		return nil, err
	}

	obj := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			// Authorino enforces host uniqueness across its watch scope, so only
			// namespace-qualified names are claimed; a bare Service name or
			// localhost would collide with another install or AuthConfig.
			"hosts": []any{
				fmt.Sprintf("%s.%s.svc", gatewayServiceName, namespace),
				fmt.Sprintf("%s.%s.svc.cluster.local", gatewayServiceName, namespace),
			},
			"authentication": authBlocks,
			"authorization":  authorization(namespace, model, machineSAs),
			"response":       response(namespace, audience, model),
		},
	}}
	obj.SetGroupVersionKind(schema.GroupVersionKind{Group: "authorino.kuadrant.io", Version: "v1beta3", Kind: "AuthConfig"})
	obj.SetName(AuthConfigName)
	obj.SetNamespace(namespace)
	obj.SetLabels(component.Identity{Name: gatewayServiceName, Component: "gateway", Instance: cr.Name}.Labels())
	return obj, nil
}

func authentication(audience string, providers []hyperfleetv1alpha1.IdentityProvider, model tenant.Model) (map[string]any, error) {
	blocks := map[string]any{
		// Selecting by scheme avoids a TokenReview on every human request and
		// issuer collisions between ServiceAccount tokens and human JWTs.
		"hyperfleet-components": map[string]any{
			"when": []any{map[string]any{
				"predicate": "request.headers['authorization'].startsWith('ServiceAccount ')",
			}},
			"kubernetesTokenReview": map[string]any{"audiences": []any{audience}},
			"credentials": map[string]any{
				"authorizationHeader": map[string]any{"prefix": "ServiceAccount"},
			},
			"overrides": map[string]any{
				"hf_system": map[string]any{"value": "true"},
			},
		},
	}

	issuers := map[string]string{}
	for _, p := range providers {
		issuer := lowercaseHost(p.Issuer)
		if first, dup := issuers[issuer]; dup {
			return nil, fmt.Errorf("identity providers %q and %q have the same issuer once the host is lowercased", first, p.Issuer)
		}
		issuers[issuer] = p.Issuer

		// A misspelled key must not silently fall back to the preset: for a
		// provider whose administrators control the claim, that would defeat the
		// tenant binding the CR meant to set.
		for _, key := range slices.Sorted(maps.Keys(p.TenantDimensions)) {
			if !slices.ContainsFunc(model, func(d tenant.Dimension) bool { return d.Key == key }) {
				return nil, fmt.Errorf("identity provider %q sets unknown tenant dimension %q; the bundle defines %s", p.Issuer, key, dimensionKeys(model))
			}
		}

		identityClaim := p.IdentityClaim
		if identityClaim == "" {
			identityClaim = "sub"
		}
		// Overrides, not defaults: defaults keep a token-supplied value, which
		// would let an identity provider admin mint an arbitrary tenant.
		overrides := map[string]any{
			"hf_system":   map[string]any{"value": "false"},
			"hf_identity": expression(fmt.Sprintf("auth.identity[%q]", identityClaim)),
		}
		for _, d := range model {
			// An unlisted dimension has no override on purpose: it falls back to
			// the bundle preset, so the token's own claim of that name is
			// trusted and reaches the wristband. Documented in the CR reference.
			src := p.TenantDimensions[d.Key]
			switch {
			case src.Value != "":
				overrides[d.Claim] = literal(src.Value)
			// A claim with the dimension's own name already reaches the
			// wristband from the token; only a renamed one needs mapping.
			case src.Claim != "" && src.Claim != d.Claim:
				overrides[d.Claim] = expression(guardedClaim(src.Claim))
			}
		}

		blocks[providerKey(issuer, blocks)] = map[string]any{
			"when": []any{map[string]any{
				"predicate": "request.headers['authorization'].startsWith('Bearer ')",
			}},
			"jwt": map[string]any{
				// issuerUrl discovers OIDC configuration/JWKS; issuer and audiences
				// enforce the JWT claims.
				"issuerUrl": issuer,
				"issuer":    issuer,
				"audiences": []any{audience},
			},
			// No credentials: Authorino defaults to the Authorization header with
			// the Bearer prefix.
			"overrides": overrides,
		}
	}
	return blocks, nil
}

func authorization(namespace string, model tenant.Model, machineSAs []string) map[string]any {
	humanOnly := when(selHFSystem, "neq", systemTrue)

	// Audience alone is not a credential: machine callers must be one of the
	// listed ServiceAccounts in this namespace.
	quoted := make([]string, len(machineSAs))
	for i, sa := range machineSAs {
		quoted[i] = regexp.QuoteMeta(sa)
	}
	subjectPattern := fmt.Sprintf("^system:serviceaccount:%s:(%s)$", regexp.QuoteMeta(namespace), strings.Join(quoted, "|"))

	rules := map[string]any{
		// hf_identity is an override, so the type check sees the value the
		// wristband will carry: a numeric sub or a groups array is denied here.
		"require-identity": map[string]any{
			"when": []any{humanOnly},
			"patternMatching": map[string]any{"patterns": []any{
				when(selHFIdentity, "neq", ""),
				map[string]any{"predicate": `type(auth.identity["hf_identity"]) == string`},
			}},
		},
		"restrict-system-subjects": rule(when(selHFSystem, "eq", systemTrue), "auth.identity.user.username", "matches", subjectPattern),
	}
	for _, d := range model {
		name := strings.ReplaceAll(d.Key, "_", "-")
		if d.Required {
			rules["require-"+name] = rule(humanOnly, "auth.identity."+d.Claim, "neq", "")
		}
		// A dimension claim is one string. The API rejects any other type, and an
		// array (a groups claim mapped onto a dimension, say) has no safe single
		// value to pick, so such a token is denied here.
		rules["require-string-"+name] = predicateRule(humanOnly,
			fmt.Sprintf("!(%[1]q in auth.identity) || type(auth.identity[%[1]q]) == string", d.Claim))
	}
	return rules
}

func dimensionKeys(model tenant.Model) string {
	keys := make([]string, len(model))
	for i, d := range model {
		keys[i] = d.Key
	}
	return strings.Join(keys, ", ")
}

func response(namespace, audience string, model tenant.Model) map[string]any {
	customClaims := map[string]any{
		"aud":           literal(audience),
		"sub":           expression(fmt.Sprintf("%s == '%s' ? auth.identity.user.username : %s", selHFSystem, systemTrue, selHFIdentity)),
		claimHFIdentity: selector(selHFIdentity),
		claimHFSystem:   selector(selHFSystem),
	}
	for _, d := range model {
		customClaims[d.Claim] = expression(guardedClaim(d.Claim))
	}

	// No response headers: in EDGE+API the API ignores tenant and system
	// headers and trusts only these wristband claims.
	return map[string]any{
		"success": map[string]any{
			"dynamicMetadata": map[string]any{
				"wristband": map[string]any{
					"wristband": map[string]any{
						"issuer":        WristbandIssuer(namespace),
						"tokenDuration": int64(300),
						"signingKeyRefs": []any{map[string]any{
							"name": WristbandSigningKeySecretName,
							// The signing Secret must hold an RSA key for RS256.
							"algorithm": "RS256",
						}},
						"customClaims": customClaims,
					},
				},
			},
		},
	}
}

var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// lowercaseHost lowercases the scheme and host, which URLs treat as
// case-insensitive, and leaves the case-sensitive rest untouched. The issuer is
// compared with the token's iss claim as-is, so the host is lowercased once here
// for every use.
func lowercaseHost(issuer string) string {
	scheme, rest, ok := strings.Cut(issuer, "://")
	if !ok {
		return issuer
	}
	host, path := rest, ""
	if i := strings.Index(rest, "/"); i >= 0 {
		host, path = rest[:i], rest[i:]
	}
	return strings.ToLower(scheme) + "://" + strings.ToLower(host) + path
}

func providerKey(issuer string, blocks map[string]any) string {
	raw := strings.ToLower(issuer)
	if u, err := url.Parse(issuer); err == nil {
		raw = strings.ToLower(u.Host + u.Path)
	}
	key := strings.Trim(nonAlnum.ReplaceAllString(raw, "-"), "-")
	if _, taken := blocks[key]; taken {
		sum := sha256.Sum256([]byte(issuer))
		base := key + "-" + hex.EncodeToString(sum[:])[:8]
		// The suffixed key can itself belong to an issuer that sanitizes to it.
		key = base
		for n := 2; ; n++ {
			if _, taken := blocks[key]; !taken {
				return key
			}
			key = fmt.Sprintf("%s-%d", base, n)
		}
	}
	return key
}

// guardedClaim yields "" for an absent claim; reading it unguarded renders a
// nil value that poisons the API's tenant containment matching.
func guardedClaim(claim string) string {
	return fmt.Sprintf("%[1]q in auth.identity ? auth.identity[%[1]q] : ''", claim)
}

func when(sel, operator, value string) map[string]any {
	return map[string]any{"selector": sel, "operator": operator, "value": value}
}

func literal(v string) map[string]any    { return map[string]any{"value": v} }
func expression(e string) map[string]any { return map[string]any{"expression": e} }
func selector(s string) map[string]any   { return map[string]any{"selector": s} }

func predicateRule(guard map[string]any, predicate string) map[string]any {
	return map[string]any{
		"when":            []any{guard},
		"patternMatching": map[string]any{"patterns": []any{map[string]any{"predicate": predicate}}},
	}
}

func rule(guard map[string]any, sel, operator, value string) map[string]any {
	return map[string]any{
		"when":            []any{guard},
		"patternMatching": map[string]any{"patterns": []any{when(sel, operator, value)}},
	}
}
