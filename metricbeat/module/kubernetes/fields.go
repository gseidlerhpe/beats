// Licensed to Elasticsearch B.V. under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Elasticsearch B.V. licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

// Code generated by beats/dev-tools/cmd/asset/asset.go - DO NOT EDIT.

package kubernetes

import (
	"github.com/elastic/beats/libbeat/asset"
)

func init() {
	if err := asset.SetFields("metricbeat", "kubernetes", Asset); err != nil {
		panic(err)
	}
}

// Asset returns asset data
func Asset() string {
	return "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"
}
