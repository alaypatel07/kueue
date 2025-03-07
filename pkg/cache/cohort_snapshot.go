/*
Copyright 2024 The Kubernetes Authors.

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

package cache

import (
	"k8s.io/apimachinery/pkg/util/sets"
)

type CohortSnapshot struct {
	Name    string
	Members sets.Set[*ClusterQueueSnapshot]

	ResourceNode ResourceNode

	// AllocatableResourceGeneration equals to
	// the sum of allocatable generation among its members.
	AllocatableResourceGeneration int64
}

// The methods below implement hierarchicalResourceNode interface.

func (c *CohortSnapshot) HasParent() bool {
	return false
}

func (c *CohortSnapshot) getResourceNode() ResourceNode {
	return c.ResourceNode
}

func (c *CohortSnapshot) parentHRN() hierarchicalResourceNode {
	return nil
}
