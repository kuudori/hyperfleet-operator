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

package component_test

import (
	"testing"

	. "github.com/onsi/gomega"

	"github.com/openshift-hyperfleet/hyperfleet-operator/internal/component"
)

func TestIdentity(t *testing.T) {
	g := NewWithT(t)
	const name, comp, instance = "hyperfleet-api", "api", "cluster"
	id := component.Identity{Name: name, Component: comp, Instance: instance}

	g.Expect(id.Labels()).To(Equal(map[string]string{
		"app.kubernetes.io/name":       name,
		"app.kubernetes.io/instance":   instance,
		"app.kubernetes.io/component":  comp,
		"app.kubernetes.io/part-of":    "hyperfleet",
		"app.kubernetes.io/managed-by": "hyperfleet-operator",
	}))

	// A Deployment selector cannot change once created.
	g.Expect(id.SelectorLabels()).To(Equal(map[string]string{
		"app.kubernetes.io/name":      name,
		"app.kubernetes.io/instance":  instance,
		"app.kubernetes.io/component": comp,
	}))
	for key, value := range id.SelectorLabels() {
		g.Expect(id.Labels()).To(HaveKeyWithValue(key, value))
	}

	meta := id.ObjectMeta("hyperfleet-api-config", "hyperfleet-system")
	g.Expect(meta.Name).To(Equal("hyperfleet-api-config"))
	g.Expect(meta.Namespace).To(Equal("hyperfleet-system"))
	g.Expect(meta.Labels).To(Equal(id.Labels()))

	// Fresh map per call: adding a label must not affect another operand.
	id.Labels()["extra"] = "x"
	g.Expect(id.Labels()).NotTo(HaveKey("extra"))
}
