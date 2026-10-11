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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Algorithm selects which rate-limiting algorithm the injected proxy runs.
// +kubebuilder:validation:Enum=tokenbucket;fixedwindow
type Algorithm string

const (
	AlgorithmTokenBucket Algorithm = "tokenbucket"
	AlgorithmFixedWindow Algorithm = "fixedwindow"
)

// Store selects where limiter state lives.
// +kubebuilder:validation:Enum=memory;redis
type Store string

const (
	StoreMemory Store = "memory"
	StoreRedis  Store = "redis"
)

// KeyType selects what requests are bucketed by.
// +kubebuilder:validation:Enum=ip;apikey
type KeyType string

const (
	KeyIP     KeyType = "ip"
	KeyAPIKey KeyType = "apikey"
)

// TokenBucketSpec configures the tokenbucket algorithm.
type TokenBucketSpec struct {
	// requests is how many tokens are added to the bucket each period.
	//
	// +required
	// +kubebuilder:validation:Minimum=1
	Requests int32 `json:"requests"`

	// period is the interval over which requests tokens are added, written as a Go
	// duration such as 1s, 500ms or 1m. Together with requests it sets the sustained
	// rate, so one request per 10s is a tenth of a request per second.
	//
	// +optional
	// +kubebuilder:default="1s"
	// +kubebuilder:validation:XValidation:rule="duration(self) > duration('0s')",message="period must be a positive Go duration, such as 1s or 1m"
	Period *metav1.Duration `json:"period,omitempty"`

	// burst is the bucket capacity: the largest number of requests that can be served
	// back to back after an idle stretch.
	//
	// +required
	// +kubebuilder:validation:Minimum=1
	Burst int32 `json:"burst"`
}

// FixedWindowSpec configures the fixedwindow algorithm.
type FixedWindowSpec struct {
	// requests is how many requests are allowed in each window.
	//
	// +required
	// +kubebuilder:validation:Minimum=1
	Requests int32 `json:"requests"`

	// window is the window length, written as a Go duration such as 30s or 1m. Every
	// window starts empty, so a client may spend its whole allowance the instant one
	// rolls over.
	//
	// +required
	// +kubebuilder:validation:XValidation:rule="duration(self) > duration('0s')",message="window must be a positive Go duration, such as 30s or 1m"
	Window metav1.Duration `json:"window"`
}

// RateLimitPolicySpec defines the desired state of RateLimitPolicy.
// +kubebuilder:validation:XValidation:rule="self.algorithm == 'tokenbucket' ? has(self.tokenBucket) && !has(self.fixedWindow) : has(self.fixedWindow) && !has(self.tokenBucket)",message="the parameter block must be the one named by algorithm: tokenBucket for tokenbucket, fixedWindow for fixedwindow"
// +kubebuilder:validation:XValidation:rule="!has(self.keyHeader) || self.key == 'apikey'",message="keyHeader may only be set when key is apikey"
type RateLimitPolicySpec struct {
	// algorithm selects the limiter and therefore which parameter block must be set.
	// +required
	Algorithm Algorithm `json:"algorithm"`

	// tokenBucket configures the tokenbucket algorithm.
	// Required when algorithm is tokenbucket, and rejected otherwise.
	// +optional
	TokenBucket *TokenBucketSpec `json:"tokenBucket,omitempty"`

	// fixedWindow configures the fixedwindow algorithm.
	// Required when algorithm is fixedwindow, and rejected otherwise.
	// +optional
	FixedWindow *FixedWindowSpec `json:"fixedWindow,omitempty"`

	// store selects where limiter state is kept.
	// memory gives every pod its own buckets, so the limit is enforced per replica.
	// redis shares one set of buckets across every pod using this policy, and makes the operator provision a Redis Deployment, Service and Secret owned by this object.
	//
	// +required
	Store Store `json:"store"`

	// key selects what requests are bucketed by. ip uses the client address read from the
	// X-Forwarded-For header, which the ingress is expected to overwrite with the address
	// it saw; a request arriving without that header is rejected with 400. apikey uses
	// the value of keyHeader instead.
	//
	// +optional
	// +kubebuilder:default="ip"
	Key KeyType `json:"key,omitempty"`

	// keyHeader names the header carrying the API key, and may only be set when key is
	// apikey. If left unset, the proxy's default of X-API-Key applies.
	//
	// +optional
	// +kubebuilder:validation:MinLength=1
	KeyHeader string `json:"keyHeader,omitempty"`

	// failOpen decides what a store outage does: true turns limiter errors into allows, false into 500s.
	//
	// +optional
	// +kubebuilder:default=true
	FailOpen *bool `json:"failOpen,omitempty"`
}

// RateLimitPolicyStatus defines the observed state of RateLimitPolicy.
type RateLimitPolicyStatus struct {
	// observedGeneration is the .metadata.generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions holds the policy's Ready condition, which is True once its backing resources match the spec.
	// For a memory-store policy that is immediately, with no resources to create.
	//
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=rlp
// +kubebuilder:printcolumn:name="Algorithm",type=string,JSONPath=`.spec.algorithm`
// +kubebuilder:printcolumn:name="Store",type=string,JSONPath=`.spec.store`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// RateLimitPolicy is the Schema for the ratelimitpolicies API
type RateLimitPolicy struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of RateLimitPolicy
	// +required
	Spec RateLimitPolicySpec `json:"spec"`

	// status defines the observed state of RateLimitPolicy
	// +optional
	Status RateLimitPolicyStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// RateLimitPolicyList contains a list of RateLimitPolicy
type RateLimitPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []RateLimitPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &RateLimitPolicy{}, &RateLimitPolicyList{})
		return nil
	})
}
