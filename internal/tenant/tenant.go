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

// Package tenant defines the per-bundle tenant dimension presets shared by the
// gateway AuthConfig and the API's tenant configuration, so the two cannot drift.
//
// A provider that does not configure a dimension falls back to its preset: the
// token claim named by Dimension.Claim is trusted as is. Changing a preset
// changes that behavior for every such provider.
package tenant

import (
	"fmt"

	hyperfleetv1alpha1 "github.com/openshift-hyperfleet/hyperfleet-operator/api/v1alpha1"
)

type Dimension struct {
	// Key names the dimension in spec.authentication tenantDimensions and in the API.
	Key   string
	Claim string
	// Required dimensions must be non-empty for human callers.
	Required bool
}

type Model []Dimension

func ForBundle(b hyperfleetv1alpha1.BundleType) (Model, error) {
	switch b {
	case hyperfleetv1alpha1.BundleCloudCAPI:
		return Model{
			{Key: "tenancy_ocid", Claim: "tenancy_ocid", Required: true},
			{Key: "compartment_id", Claim: "compartment_id"},
		}, nil
	case hyperfleetv1alpha1.BundleOnPremAgent:
		return Model{
			{Key: "org", Claim: "org_id", Required: true},
			{Key: "project", Claim: "project_id"},
		}, nil
	default:
		return nil, fmt.Errorf("no tenant model for bundle %q", b)
	}
}
