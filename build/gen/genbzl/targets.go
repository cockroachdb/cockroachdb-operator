// Copyright 2022 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package main

import "text/template"

// queryData is data passed to the targets templates.
type queryData struct {
	// All is a bazel query expression representing all of the targets
	// and files in the workspace.
	All string
}

var targets = []struct {
	target        target
	query         string
	doNotGenerate bool
}{
	{
		target: "go_protobuf",
		query: `
kind(go_proto_library, {{ .All }})`,
	},
	{
		target: "ts_protobuf",
		query: `
filter(".*_proto", kind(js_library, {{ .All }}))`,
	},
	{
		target: "gomock",
		query: `
labels("out", kind("_gomock_prog_exec rule", {{ .All }}))`,
	},
	{
		target: "go_binaries",
		query: `
kind(go_binary, {{ .All }}) except (filter(".*gomock_prog_bin|static", kind(go_binary, {{ .All }})))`,
	},
}

var queryTemplates = template.New("queries")

func init() {
	for _, q := range targets {
		template.Must(queryTemplates.New(string(q.target)).Parse(q.query))
	}
}
