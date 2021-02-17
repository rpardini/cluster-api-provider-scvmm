/*


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

package v1alpha3

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ScvmmMachineTemplateSpec defines the desired state of ScvmmMachineTemplate
type ScvmmMachineTemplateSpec struct {
	Template ScvmmMachineTemplateResource `json:"template"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:path=scvmmmachinetemplates,scope=Namespaced,categories=cluster-api
// +kubebuilder:storageversion

// ScvmmMachineTemplate is the Schema for the scvmmmachinetemplates API
type ScvmmMachineTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ScvmmMachineTemplateSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// ScvmmMachineTemplateList contains a list of ScvmmMachineTemplate
type ScvmmMachineTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ScvmmMachineTemplate `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ScvmmMachineTemplate{}, &ScvmmMachineTemplateList{})
}

// ScvmmMachineTemplateResource describes the data needed to create a ScvmmMachine from a template
type ScvmmMachineTemplateResource struct {
	ObjectMeta `json:"metadata,omitempty"`
	Spec       ScvmmMachineSpec `json:"spec"`
}

// Copy of ObjectMeta, with only labels and annotations for now
type ObjectMeta struct {
	// Map of string keys and values that can be used to organize and categorize
	// (scope and select) objects. May match selectors of replication controllers
	// and services.
	// More info: http://kubernetes.io/docs/user-guide/labels
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// Annotations is an unstructured key value map stored with a resource that may be
	// set by external tools to store and retrieve arbitrary metadata. They are not
	// queryable and should be preserved when modifying objects.
	// More info: http://kubernetes.io/docs/user-guide/annotations
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}
