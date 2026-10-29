// cockroach is a collection of packages that have been extracted from the
// cockroachdb/cockroach repo for the purpose of use within this repository
// without incurring a complete dependency on Cockroach.
//
// With any such luck, this package will eventually be replaced by an 3rd
// repository that can be shared between CRDB and CC but we're currently aiming
// to server the vendoring of CRDB within CC.
//
// Packages that MUST exist should be entered into this package. For example,
// shared protocols between CC and CRDB should live here. Helpful utility
// functions should be assimilated into the CC codebase elsewhere or into a 3rd
// party repository.
//
// Each subpackage added should add a new entry herein the below template:
// packagename: (TREEISH, /dir/in/crdb)
// Brief description of the package and why it was extracted.
// How the extraction was performed.
// Modifications made, if any.
//
// logpb: (9af72fb569590f6cd6d80e91ef9956348c709497, pkg/util/log/logpb)
// logpb is the protocol buffer that defines CockroachDB's logging
// configuration. CC uses it for configuration generation and validation.
// Like all protocol buffers extract from CRDB, bazel is used to pull the
// .proto file and then to generate the go code.
// No modifications.
//
// logconfig: (9af72fb569590f6cd6d80e91ef9956348c709497, pkg/util/log/logconfig)
// Similar to logpb but contains defaults, constants, and validation code.
// Extracted using git archive run within CRDB:
//
//	git archive --format tar v22.2.2 pkg/util/log/logconfig/ | tar -xf - --strip=4 -C $MSO_REPO/pkg/cockroach/logconfig
//
// gen.go has been deleted. BUILD.bazel has been replaced with a CC generated one.
// Imports of logpb have been replaced with CC's logpb and import of
// humanizeutil have been replaced with go-humanize.
// MakeIssueURL has been removed and replaced with "REMOVED".
package cockroach
