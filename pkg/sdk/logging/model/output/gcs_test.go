// Copyright © 2019 Banzai Cloud
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package output_test

import (
	"testing"

	"github.com/kube-logging/logging-operator/pkg/sdk/logging/model/output"
	"github.com/kube-logging/logging-operator/pkg/sdk/logging/model/render"
	"github.com/ghodss/yaml"
	"github.com/stretchr/testify/require"
)

func TestGCS(t *testing.T) {
	CONFIG := []byte(`
project: logging-example
bucket: banzai-log-test
path: logs/${tag}/%Y/%m/%d/
buffer:
  timekey: 1m
  timekey_wait: 30s
  timekey_use_utc: true
`)
	expected := `
  <match **>
    @type gcs
    @id test
    bucket banzai-log-test
    path logs/${tag}/%Y/%m/%d/
    project logging-example
    <buffer tag,time>
      @type file
	  chunk_limit_size 8MB
      path /buffers/test.*.buffer
      retry_forever true
      timekey 1m
      timekey_use_utc true
      timekey_wait 30s
    </buffer>
  </match>
`
	g := &output.GCSOutput{}
	require.NoError(t, yaml.Unmarshal(CONFIG, g))
	test := render.NewOutputPluginTest(t, g)
	test.DiffResult(expected)
}
