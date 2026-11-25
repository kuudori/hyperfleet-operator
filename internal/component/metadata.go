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

package component

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	LabelName      = "app.kubernetes.io/name"
	LabelInstance  = "app.kubernetes.io/instance"
	LabelComponent = "app.kubernetes.io/component"
	LabelPartOf    = "app.kubernetes.io/part-of"
	LabelManagedBy = "app.kubernetes.io/managed-by"

	ManagedBy = "hyperfleet-operator"
	PartOf    = "hyperfleet"
)

// Identity is the one source of every operand's labels and selectors, so they
// cannot drift between operands or components.
type Identity struct {
	Name      string
	Component string
	Instance  string
}

func (i Identity) Labels() map[string]string {
	labels := i.SelectorLabels()
	labels[LabelPartOf] = PartOf
	labels[LabelManagedBy] = ManagedBy
	return labels
}

// SelectorLabels is the subset used by selectors. A Deployment selector cannot
// change after creation: add new labels to Labels, never here.
func (i Identity) SelectorLabels() map[string]string {
	return map[string]string{
		LabelName:      i.Name,
		LabelInstance:  i.Instance,
		LabelComponent: i.Component,
	}
}

func (i Identity) ObjectMeta(name, namespace string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: i.Labels()}
}
