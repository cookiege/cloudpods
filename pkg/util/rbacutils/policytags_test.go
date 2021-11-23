// Copyright 2019 Yunion
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package rbacutils

import (
	"testing"

	"yunion.io/x/onecloud/pkg/util/tagutils"
)

func TestSPolicy_Contains(t *testing.T) {
	cases := []struct {
		p1    SPolicy
		p2    SPolicy
		p1cp2 bool
		p2cp1 bool
	}{
		{
			p1: SPolicy{
				Rules: TPolicy{
					{
						Service: WILD_MATCH,
						Result:  Allow,
					},
				},
				DomainTags:   nil,
				ProjectTags:  nil,
				ResourceTags: nil,
			},
			p2: SPolicy{
				Rules: TPolicy{
					{
						Service: "compute",
						Result:  Allow,
					},
				},
				DomainTags: nil,
				ProjectTags: tagutils.TTagSet{
					tagutils.STag{
						Key:   "project",
						Value: "a",
					},
				},
				ResourceTags: nil,
			},
			p1cp2: true,
			p2cp1: false,
		},
		{
			p1: SPolicy{
				Rules: TPolicy{
					{
						Service:  "compute",
						Resource: "servers",
						Result:   Allow,
					},
					{
						Service:  "compute",
						Resource: "servers",
						Action:   "create",
						Result:   Deny,
					},
					{
						Service:  "compute",
						Resource: "servers",
						Action:   "delete",
						Result:   Deny,
					},
				},
				DomainTags: nil,
				ProjectTags: tagutils.TTagSet{
					tagutils.STag{
						Key:   "project",
						Value: "a",
					},
					tagutils.STag{
						Key:   "env",
						Value: "test",
					},
				},
				ResourceTags: nil,
			},
			p2: SPolicy{
				Rules: TPolicy{
					{
						Service:  "compute",
						Resource: "servers",
						Action:   "get",
						Result:   Allow,
					},
					{
						Service:  "compute",
						Resource: "servers",
						Action:   "list",
						Result:   Allow,
					},
				},
				DomainTags: nil,
				ProjectTags: tagutils.TTagSet{
					tagutils.STag{
						Key:   "project",
						Value: "a",
					},
				},
				ResourceTags: nil,
			},
			p1cp2: false,
			p2cp1: false,
		},
	}
	for i, c := range cases {
		got := c.p1.Contains(c.p2)
		if got != c.p1cp2 {
			t.Errorf("[%d] p1 contains p2 want %v got %v", i, c.p1cp2, got)
		}
		got = c.p2.Contains(c.p1)
		if got != c.p2cp1 {
			t.Errorf("[%d] p2 contains p1 want %v got %v", i, c.p2cp1, got)
		}
	}
}
