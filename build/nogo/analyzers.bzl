# This file contains a list of all possible analyzers that are runnable via
# go_rules' nogo (https://github.com/bazelbuild/rules_go/blob/master/go/nogo.rst).
# Analyzers are divided into top level "groups" and exported as lists.
#
# FIRSTPARTY_ANALYZERS are analyzers contained in this codebase.
# GOVET_ANALYZERS are analyzers built into the go tool chain.
# STATICHECK_ANALYZERS are analyzers defined by the staticcheck helper.
#
# Each grouping contains the full list of possible analyzers. Disabled
# analyzers are commented out with a justification, even in the case of
# permanant disablement. This makes it easy to discover what analyzers are
# running as well as what analyzers are NOT running.

# govet_analyzers is a helper to transform a list of analyzer names into a
# valid list of bazel targets.
def govet_analyzers(analyzers):
    name_overrides = {
        "composites": "composite",
        "copylocks": "copylock",
    }
    return [
        "@org_golang_x_tools//go/analysis/passes/" + name_overrides.get(analyzer, analyzer)
        for analyzer in analyzers
    ]

# firstparty_analyzers is a helper to transform a list of analyzer names into a
# valid list of bazel targets.
def firstparty_analyzers(analyzers):
    return [
        "//pkg/testutils/lint/analysis/" + analyzer
        for analyzer in analyzers
    ]

FIRSTPARTY_ANALYZERS = [
    "importcheck",
    "sqlsafe",
    "timeequality",
    "cerrwrapping",
    "deferspanend",
    "testskip",
]

GOVET_ANALYZERS = [
    "asmdecl",
    "assign",
    "atomic",
    "atomicalign",
    "bools",
    "buildssa",
    "buildtag",
    # Causes build errors with Cockroach for some reason.
    # "@org_golang_x_tools//go/analysis/passes/cgocall",
    "composites",
    "copylocks",
    "ctrlflow",
    "deepequalerrors",
    "errorsas",
    # Somewhat noisey but doesn't warrent fixing.
    # "@org_golang_x_tools//go/analysis/passes/fieldalignment",
    "findcall",
    "framepointer",
    "httpresponse",
    "ifaceassert",
    "inspect",
    "loopclosure",
    "lostcancel",
    "nilfunc",
    "nilness",
    "pkgfact",
    "printf",
    "reflectvaluecompare",
    # Currently disabled in roachvet.
    # "@org_golang_x_tools//go/analysis/passes/shadow:shadow",
    "shift",
    "sigchanyzer",
    "sortslice",
    "stdmethods",
    "stringintconv",
    "structtag",
    "testinggoroutine",
    "tests",
    "unmarshal",
    "unreachable",
    "unsafeptr",
    "unusedresult",
    # Causes false positives.
    # "@org_golang_x_tools//go/analysis/passes/unusedwrite",
    "usesgenerics",
]

# TODO: Many static check rules are disabled as golangcilint was not
# appropriately run all checks. Some were disabled and others simply didn't
# run. Each disabled rule will have to be re-evaluated and either fixed or have
# a justification for being disabled added.
STATICHECK_ANALYZERS = [
    # "QF1001", Apply De Morgan’s law
    # "QF1002", Convert untagged switch to tagged switch
    # "QF1003", Convert if/else-if chain to tagged switch
    # "QF1004", Use strings.ReplaceAll instead of strings.Replace with n == -1
    # "QF1005", Expand call to math.Pow
    # "QF1006", Lift if+break into loop condition
    # "QF1007", Merge conditional assignment into variable declaration
    # "QF1008", Omit embedded fields from selector expression
    "QF1009",
    "QF1010",
    # "QF1011", Omit redundant type from variable declaration
    "QF1012",
    "S1000",
    "S1001",
    "S1002",
    "S1003",
    "S1004",
    "S1005",
    "S1006",
    "S1007",
    "S1008",
    "S1009",
    "S1010",
    "S1011",
    "S1012",
    "S1016",
    "S1017",
    "S1018",
    "S1019",
    "S1020",
    "S1021",
    "S1023",
    "S1024",
    "S1025",
    "S1028",
    "S1029",
    "S1030",
    "S1031",
    "S1032",
    "S1033",
    "S1034",
    "S1035",
    "S1036",
    "S1037",
    "S1038",
    # "S1039", Unnecessary use of fmt.Sprint
    "S1040",
    "SA1000",
    "SA1001",
    "SA1002",
    "SA1003",
    "SA1004",
    "SA1005",
    "SA1006",
    "SA1007",
    "SA1008",
    "SA1010",
    "SA1011",
    "SA1012",
    "SA1013",
    "SA1014",
    "SA1015",
    "SA1016",
    "SA1017",
    "SA1018",
    # "SA1019", Using a deprecated function, variable, constant or field
    "SA1020",
    "SA1021",
    "SA1023",
    "SA1024",
    "SA1025",
    "SA1026",
    "SA1027",
    "SA1028",
    "SA1029",
    "SA1030",
    "SA2000",
    "SA2001",
    "SA2002",
    "SA2003",
    "SA3000",
    "SA3001",
    "SA4000",
    "SA4001",
    "SA4003",
    "SA4004",
    "SA4005",
    "SA4006",
    "SA4008",
    "SA4009",
    "SA4010",
    # "SA4011", Break statement with no effect. Did you mean to break out of an outer loop?
    "SA4012",
    "SA4013",
    "SA4014",
    "SA4015",
    "SA4016",
    "SA4017",
    "SA4018",
    "SA4019",
    "SA4020",
    "SA4021",
    "SA4022",
    "SA4023",
    "SA4024",
    "SA4025",
    "SA4026",
    "SA4027",
    "SA4028",
    "SA4029",
    "SA4030",
    "SA4031",
    "SA5000",
    "SA5001",
    "SA5002",
    "SA5003",
    "SA5004",
    "SA5005",
    "SA5007",
    "SA5008",
    "SA5009",
    "SA5010",
    "SA5011",
    "SA5012",
    "SA6000",
    "SA6001",
    "SA6002",
    "SA6003",
    # "SA6005", Inefficient string comparison with strings.ToLower or strings.ToUpper
    "SA9001",
    "SA9002",
    "SA9003",
    "SA9004",
    "SA9005",
    "SA9006",
    "SA9007",
    "SA9008",
    # "ST1000", Incorrect or missing package comment
    "ST1001",
    # "ST1003", Poorly chosen identifier
    # "ST1005", Incorrectly formatted error string
    # "ST1006", Poorly chosen receiver name
    "ST1008",
    # "ST1011", Poorly chosen name for variable of type time.Duration
    "ST1012",
    # "ST1013", Should use constants for HTTP error codes, not magic numbers
    "ST1015",
    # "ST1016", Use consistent method receiver names
    "ST1017",
    "ST1018",
    # "ST1019", Importing the same package multiple times
    # "ST1020", The documentation of an exported function should start with the function’s name
    # "ST1021", The documentation of an exported type should start with type’s name
    # "ST1022", The documentation of an exported variable or constant should start with variable’s name
    # "ST1023", Redundant type in variable declaration
    "U1000",
]
