// Copyright 2023 Vic Shóstak and Create Go App Contributors. All rights reserved.
// Use of this source code is governed by Apache 2.0 license
// that can be found in the LICENSE file.

package genplan

// DemoPlan builds a self-contained example plan: it generates a tiny,
// buildable Go module, probes the toolchain idempotently, optionally performs
// one policy-checked GET request, and verifies the staging tree with
// `go build ./...` before the atomic commit.
func DemoPlan(optionalURL string) *Plan {
	p := &Plan{
		Version: PlanVersion,
		Policy: Policy{
			AllowCommands: []string{"go"},
			MaxFileBytes:  DefaultMaxFileBytes,
		},
		Steps: []Step{
			{
				ID:   "write-go-mod",
				Type: StepWriteFile,
				File: &FileSpec{Path: "go.mod", Content: "module demo\n\ngo 1.23\n"},
			},
			{
				ID:          "write-main",
				Type:        StepWriteFile,
				Description: "application entry point",
				File: &FileSpec{Path: "main.go", Content: `package main

import "fmt"

func main() {
	fmt.Println("Hello from a transactionally generated project!")
}
`},
			},
			{
				ID:          "probe-toolchain",
				Type:        StepRunCommand,
				Description: "read the Go toolchain version (idempotent)",
				Run: &CommandSpec{
					Bin: "go", Args: []string{"env", "GOVERSION"},
					Idempotent:     true,
					TimeoutSeconds: 30,
				},
			},
		},
		Verify: []CommandSpec{
			// `go vet` compiles the whole tree without leaving binaries behind,
			// keeping the committed output deterministic.
			{Bin: "go", Args: []string{"vet", "./..."}, TimeoutSeconds: 120},
		},
	}

	if optionalURL != "" {
		if host := httpHostPort(optionalURL); host != "" {
			p.Policy.AllowNetwork = []string{host} // exact host:port match
			p.Steps = append(p.Steps, Step{
				ID:          "fetch-url",
				Type:        StepHTTPRequest,
				Description: "optional policy-checked GET request",
				HTTP: &HTTPSpec{
					Method: "GET", URL: optionalURL, Idempotent: true,
					ExpectStatus:   200,
					TimeoutSeconds: 30,
				},
			})
		}
	}
	return p
}
