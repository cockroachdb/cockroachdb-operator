// Copyright 2025 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package version

import (
	"cmp"
	"regexp"
	"strconv"

	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/redact"
)

var _ redact.SafeFormatter = MajorVersion{}

// A MajorVersion represents a CockroachDB major version or release series, ie "v25.1".
type MajorVersion struct {
	Year, Ordinal int
}

// ParseMajorVersion constructs a MajorVersion from a string.
func ParseMajorVersion(versionStr string) (MajorVersion, error) {
	majorVersionRE := regexp.MustCompile(`^v(0|[1-9][0-9]*)\.([1-9][0-9]*)$`)
	if !majorVersionRE.MatchString(versionStr) {
		return MajorVersion{}, errors.Newf("not a valid CockroachDB major version: %s", versionStr)
	}
	groups := majorVersionRE.FindStringSubmatch(versionStr)
	year, _ := strconv.Atoi(groups[1])
	ordinal, _ := strconv.Atoi(groups[2])
	return MajorVersion{year, ordinal}, nil
}

// MustParseMajorVersion is like ParseMajorVersion but panics on any error.
// Recommended as an initializer for global values.
func MustParseMajorVersion(versionStr string) MajorVersion {
	majorVersion, err := ParseMajorVersion(versionStr)
	if err != nil {
		panic(err)
	}
	return majorVersion
}

// Compare returns -1, 0, or +1 indicating the relative ordering of major versions.
func (m MajorVersion) Compare(o MajorVersion) int {
	if r := cmp.Compare(m.Year, o.Year); r != 0 {
		return r
	}
	return cmp.Compare(m.Ordinal, o.Ordinal)
}

func (m MajorVersion) Equals(o MajorVersion) bool {
	return m.Compare(o) == 0
}

func (m MajorVersion) LessThan(o MajorVersion) bool {
	return m.Compare(o) < 0
}

func (m MajorVersion) AtLeast(o MajorVersion) bool {
	return m.Compare(o) >= 0
}

// Empty returns true if the MajorVersion is the zero value.
func (m MajorVersion) Empty() bool {
	return m.Compare(MajorVersion{}) == 0
}

// String returns the original string passed to ParseMajorVersion.
func (m MajorVersion) String() string {
	return redact.StringWithoutMarkers(m)
}

// SafeFormat implements [redact.SafeFormatter].
func (m MajorVersion) SafeFormat(p redact.SafePrinter, _ rune) {
	p.Printf("v%d.%d", m.Year, m.Ordinal)
}
