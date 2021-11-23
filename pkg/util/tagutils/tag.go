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

package tagutils

type STag struct {
	// 标签Kye
	Key string `json:"key"`
	// 标签Value
	Value string `json:"value"`
}

func Compare(t1, t2 STag) int {
	if t1.Key < t2.Key {
		return -1
	} else if t1.Key > t2.Key {
		return 1
	}
	if t1.Value < t2.Value {
		return -1
	} else if t1.Value > t2.Value {
		return 1
	}
	return 0
}
