// Copyright 2022 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

// Command genbzl is used to generate bazel files which then get imported by
// the gen package's BUILD.bazel to facilitate hoisting these generated files
// back into the source tree.
//
// The flow is that we invoke this binary via make which writes out some bzl
// files defining lists of targets for generation and hoisting.
//
// The program assumes it will be run from the root of the bazel workspace. If
// it's not, errors will occur.
package main

import (
	"flag"
	"log"
)

var (
	outDir = flag.String(
		"out-dir", "",
		"directory in which to place the generated files",
	)
)

func main() {
	flag.Parse()
	if err := generate(*outDir); err != nil {
		log.Fatalf("failed to generate files: %v", err)
	}
}

func generate(outDir string) error {
	for _, q := range targets {
		if q.doNotGenerate {
			continue
		}
		out, err := q.target.execQuery(&queryData{All: "//..."})
		if err != nil {
			return err
		}
		if err := q.target.write(outDir, out); err != nil {
			return err
		}
	}
	return nil
}
