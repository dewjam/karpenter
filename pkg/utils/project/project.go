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

package project

import (
	"path/filepath"
	"runtime"
	"strings"
)

var (
	// Version is the karpenter app version injected during compilation
	// when using the Makefile
	Version = "unspecified"
)

func RelativeToRoot(path string) string {
	_, file, _, _ := runtime.Caller(0)
	manifestsRoot := filepath.Join(filepath.Dir(file), "..", "..", "..")
	return filepath.Join(manifestsRoot, path)
}

func GetReleaseVersion() string {
	if strings.Contains(Version, "-") {
		return "preview"
	}
	return Version
}
