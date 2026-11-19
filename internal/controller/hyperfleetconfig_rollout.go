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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift-hyperfleet/hyperfleet-operator/internal/component"
)

// configHashAnnotation is stamped on the API Deployment's pod template. When it
// changes, the Deployment controller performs a rolling update, so a config or
// referenced-secret change takes effect even though the container image is
// unchanged.
const configHashAnnotation = "hyperfleet.redhat.com/config-hash"

// hashEntry is one referenced Secret's contribution to the rollout hash: its
// resourceVersion, not its data (see referencedSecretData for why). present
// distinguishes a missing Secret (hashed as an absent discriminator, see
// computeConfigHash) from an existing one, so a Secret appearing later still
// changes the hash and rolls the pods.
type hashEntry struct {
	id      string
	present bool
	value   []byte
}

// resolveSecretEntries reads the resourceVersion of each referenced Secret and
// returns hash entries. A missing Secret is recorded as absent (not an error);
// the controller surfaces it via the Degraded condition. Each component
// declares its own references via ReferencedSecrets, so the hash is scoped to
// only the Secrets that component actually consumes.
//
// The hash covers resourceVersion, not Secret data. Hashing the actual
// credential bytes was the original design, but it turns the pod-template
// annotation — readable by anyone who can read the Deployment or its Pods, a
// much wider audience than Secret readers — into an offline oracle for
// low-entropy credentials (e.g. a weak database password): with the rendered
// config.yaml and the other connection fields typically knowable, the
// password becomes the only unknown in an otherwise-known SHA-256 preimage.
// resourceVersion changes on every write, including a rotation, which is all
// the rollout needs, and it means the operator never needs to read Secret
// payloads at all — only their metadata.
func (r *HyperFleetConfigReconciler) resolveSecretEntries(ctx context.Context, refs []component.SecretReference) ([]hashEntry, error) {
	entries := make([]hashEntry, 0, len(refs))
	// iterate over the component's referenced Secrets,
	for _, ref := range refs {
		s := &corev1.Secret{}
		key := types.NamespacedName{Name: ref.SecretName, Namespace: r.OperatorNamespace}
		if err := r.Get(ctx, key, s); err != nil {
			if apierrors.IsNotFound(err) {
				entries = append(entries, hashEntry{id: ref.Role})
				continue
			}
			return nil, fmt.Errorf("get secret %q: %w", ref.SecretName, err)
		}
		entries = append(entries, hashEntry{id: ref.Role, present: true, value: []byte(s.ResourceVersion)})
	}
	return entries, nil
}

// computeConfigHash returns a stable SHA-256 over the rendered config.yaml and
// the referenced-secret entries. Entries are sorted by id and every field is
// length-delimited with a NUL separator so distinct inputs cannot collide by
// concatenation (e.g. "ab"+"c" vs "a"+"bc").
func computeConfigHash(configYAML string, entries []hashEntry) string {
	sorted := make([]hashEntry, len(entries))
	copy(sorted, entries)
	slices.SortFunc(sorted, func(a, b hashEntry) int { return strings.Compare(a.id, b.id) })

	h := sha256.New()
	writeField := func(b []byte) {
		// Length-prefix each field so boundaries are unambiguous.
		_, _ = io.WriteString(h, fmt.Sprintf("%d:", len(b)))
		_, _ = h.Write(b)
		_, _ = h.Write([]byte{0})
	}

	writeField([]byte(configYAML))
	for _, e := range sorted {
		writeField([]byte(e.id))
		// A one-byte present/absent discriminator precedes the value so a missing
		// datum can never collide with a present value that happens to equal the
		// absent marker's bytes. Absent writes only the 0x00 tag; present writes
		// 0x01 followed by the length-delimited value.
		if e.present {
			_, _ = h.Write([]byte{1})
			writeField(e.value)
		} else {
			_, _ = h.Write([]byte{0})
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// stampConfigHash computes a rollout hash from the component's rendered
// ConfigMap data and the supplied hash entries, then writes it as a
// pod-template annotation on the component's Deployment. A change in any
// input produces a new hash, which triggers a rolling restart.
func stampConfigHash(objs []client.Object, target component.RolloutTarget, entries []hashEntry) string {
	var configYAMLs []string
	for _, entry := range target.ConfigEntries {
		for _, o := range objs {
			if cm, ok := o.(*corev1.ConfigMap); ok && cm.Name == entry.ConfigMapName {
				configYAMLs = append(configYAMLs, cm.Data[entry.ConfigFileKey])
				break
			}
		}
	}
	configYAML := strings.Join(configYAMLs, "\x00")

	hash := computeConfigHash(configYAML, entries)

	for _, o := range objs {
		dep, ok := o.(*appsv1.Deployment)
		if !ok || dep.Name != target.DeploymentName {
			continue
		}
		if dep.Spec.Template.Annotations == nil {
			dep.Spec.Template.Annotations = map[string]string{}
		}
		dep.Spec.Template.Annotations[configHashAnnotation] = hash
	}
	return hash
}
