/*
Copyright The Kubernetes Authors.

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

package dra

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	resourcev1beta1 "k8s.io/api/resource/v1beta1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta1"
)

// GetResourceRequests returns the resource requests for a workload for DRA ResourceClaimTemplates and shared ResourceClaims.
// The returned resourcelist is indexed by pod set name.
func GetResourceRequests(ctx context.Context, c client.Client, wl *kueue.Workload, lookup func(dc corev1.ResourceName) (corev1.ResourceName, bool)) (map[kueue.PodSetReference]corev1.ResourceList, error) {
	totalRequests := make(map[kueue.PodSetReference]corev1.ResourceList)

	for _, ps := range wl.Spec.PodSets {
		psRequests := make(corev1.ResourceList)

		for _, rc := range ps.Template.Spec.ResourceClaims {
			// Handle ResourceClaimTemplates
			if rc.ResourceClaimTemplateName != nil {
				templateName := *rc.ResourceClaimTemplateName
				resourceClaimTemplate := &resourcev1beta1.ResourceClaimTemplate{}
				if err := c.Get(ctx, types.NamespacedName{Name: templateName, Namespace: wl.Namespace}, resourceClaimTemplate); err != nil {
					return nil, fmt.Errorf("failed to get ResourceClaimTemplate %s: %w", templateName, err)
				}

				for _, req := range resourceClaimTemplate.Spec.Spec.Devices.Requests {
					logicalResource, ok := lookup(corev1.ResourceName(req.DeviceClassName))
					if !ok {
						return nil, fmt.Errorf("failed to find logical resource mapping for device class: %s", req.DeviceClassName)
					}

					// Add to pod set requests (no prefix)
					quantity := resource.MustParse(fmt.Sprintf("%d", req.Count))
					if existing, exists := psRequests[logicalResource]; exists {
						quantity.Add(existing)
					}
					psRequests[logicalResource] = quantity
				}
			}

			// Handle shared ResourceClaims
			if rc.ResourceClaimName != nil {
				claimName := *rc.ResourceClaimName
				resourceClaim := &resourcev1beta1.ResourceClaim{}
				if err := c.Get(ctx, types.NamespacedName{Name: claimName, Namespace: wl.Namespace}, resourceClaim); err != nil {
					return nil, fmt.Errorf("failed to get ResourceClaim %s: %w", claimName, err)
				}

				for _, req := range resourceClaim.Spec.Devices.Requests {
					logicalResource, ok := lookup(corev1.ResourceName(req.DeviceClassName))
					if !ok {
						return nil, fmt.Errorf("failed to find logical resource mapping for device class: %s", req.DeviceClassName)
					}

					// Add to pod set requests (no prefix - canonical names)
					quantity := resource.MustParse(fmt.Sprintf("%d", req.Count))
					if existing, exists := psRequests[logicalResource]; exists {
						quantity.Add(existing)
					}
					psRequests[logicalResource] = quantity
				}
			}
		}

		if len(psRequests) > 0 {
			totalRequests[ps.Name] = psRequests
		}
	}

	return totalRequests, nil
}
