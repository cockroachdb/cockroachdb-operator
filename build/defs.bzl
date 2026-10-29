load(":proto_compile.bzl", _ts_proto_library = "ts_proto_library")

# Export our custom proto compiler rules.
ts_proto_library = _ts_proto_library
