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

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MCSPCustomerSpec defines the desired state of MCSPCustomer
type MCSPCustomerSpec struct {
	// CustomerName is the name of the customer being onboarded
	// This will be used to create the namespace, policy, job etc
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	CustomerName string `json:"customerName"`
}

// MCSPCustomerStatus defines the observed state of MCSPCustomer
type MCSPCustomerStatus struct {
	// Deployed indicates whether the customer is fully deployed
	Deployed bool `json:"deployed,omitempty"`

	// Message provides information about the current state of the customer
	Message string `json:"message,omitempty"`

	// URL is the customer application endpoint
	URL string `json:"url,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:printcolumn:name="CustomerName",type="string",JSONPath=".spec.customerName"
//+kubebuilder:printcolumn:name="Deployed",type="boolean",JSONPath=".status.deployed"
//+kubebuilder:printcolumn:name="Message",type="string",JSONPath=".status.message"
//+kubebuilder:printcolumn:name="URL",type="string",JSONPath=".status.url"
//+kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// MCSPCustomer is the Schema for the mcspcustomers API
type MCSPCustomer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MCSPCustomerSpec   `json:"spec,omitempty"`
	Status MCSPCustomerStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// MCSPCustomerList contains a list of MCSPCustomer
type MCSPCustomerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MCSPCustomer `json:"items"`
}

func init() {
	SchemeBuilder.Register(&MCSPCustomer{}, &MCSPCustomerList{})
}