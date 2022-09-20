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

package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/aws/karpenter/pkg/utils/options"
)

var docsLinks = map[string]string{
	"https://karpenter.sh/unspecified/aws/provisioning/#pod-eni-security-groups-for-pods": "[Pod ENI documentation](../../aws/provisioning/#pod-eni-security-groups-for-pods)",
}

func main() {
	if len(os.Args) != 2 {
		log.Fatalf("Usage: %s path/to/markdown.md", os.Args[0])
	}
	outputFileName := os.Args[1]
	mdFile, err := os.ReadFile(outputFileName)
	if err != nil {
		log.Printf("Can't read %s file: %v", os.Args[1], err)
		os.Exit(2)
	}

	genStart := "[comment]: <> (the content below is generated from hack/docs/configuration_gen_docs.go)"
	genEnd := "[comment]: <> (end docs generated content from hack/docs/configuration_gen_docs.go)"
	startDocSections := strings.Split(string(mdFile), genStart)
	if len(startDocSections) != 2 {
		log.Fatalf("expected one generated comment block start but got %d", len(startDocSections)-1)
	}
	endDocSections := strings.Split(string(mdFile), genEnd)
	if len(endDocSections) != 2 {
		log.Fatalf("expected one generated comment block end but got %d", len(endDocSections)-1)
	}
	topDoc := fmt.Sprintf("%s%s\n\n", startDocSections[0], genStart)
	bottomDoc := fmt.Sprintf("\n%s%s", genEnd, endDocSections[1])

	opts := options.New()

	envVarsBlock := "| Environment Variable | CLI Flag | Description |\n"
	envVarsBlock += "|--|--|--|\n"
	opts.VisitAll(func(f *flag.Flag) {
		line := fmt.Sprintf("| %s | %s | %s|\n", strings.ReplaceAll(strings.ToUpper(f.Name), "-", "_"), "\\-\\-"+f.Name, f.Usage)
		if f.DefValue != "" {
			line = fmt.Sprintf("| %s | %s | %s (default = %s)|\n", strings.ReplaceAll(strings.ToUpper(f.Name), "-", "_"), "\\-\\-"+f.Name, f.Usage, f.DefValue)
		}
		envVarsBlock += insertDocsLinks(line)

	})

	log.Println("writing output to", outputFileName)
	f, err := os.Create(outputFileName)
	if err != nil {
		log.Fatalf("unable to open %s to write generated output: %v", outputFileName, err)
	}
	f.WriteString(topDoc + envVarsBlock + bottomDoc)
}

func insertDocsLinks(s string) string {
	for k, v := range docsLinks {
		s = strings.ReplaceAll(s, k, v)
	}
	return s

}
