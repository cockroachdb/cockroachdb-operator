load("@bazel_tools//tools/build_defs/repo:http.bzl", "http_archive", "http_file")

http_archive(
    name = "bazel_features",
    sha256 = "2f057dd02098a106095ea291b4344257398a059eadb2c74cc470de0f9664dccd",
    strip_prefix = "bazel_features-1.28.0",
    url = "https://github.com/bazel-contrib/bazel_features/releases/download/v1.28.0/bazel_features-v1.28.0.tar.gz",
)

load("@bazel_features//:deps.bzl", "bazel_features_deps")

bazel_features_deps()

# Load go bazel tools. This gives us access to the go bazel SDK/toolchains.
http_archive(
    name = "io_bazel_rules_go",
    sha256 = "a729c8ed2447c90fe140077689079ca0acfb7580ec41637f312d650ce9d93d96",
    urls = [
        "https://github.com/bazel-contrib/rules_go/releases/download/v0.57.0/rules_go-v0.57.0.zip",
    ],
)

# Load rules for packaging. This gives us access to bazel rules to create packages (tar, deb, etc) from build targets
http_archive(
    name = "rules_pkg",
    sha256 = "8f9ee2dc10c1ae514ee599a8b42ed99fa262b757058f65ad3c384289ff70c4b8",
    urls = [
        "https://mirror.bazel.build/github.com/bazelbuild/rules_pkg/releases/download/0.9.1/rules_pkg-0.9.1.tar.gz",
        "https://github.com/bazelbuild/rules_pkg/releases/download/0.9.1/rules_pkg-0.9.1.tar.gz",
    ],
)

# zlib is dependend on by (at least) protobuf. the version pulled by protobuf_deps()
# fails to compile on some macos machines (depending on Xcode version?), so we use
# the latest available (as of 2025-04-01) which compiles cleanly
http_archive(
    name = "zlib",
    build_file = "@com_google_protobuf//:third_party/zlib.BUILD",
    sha256 = "9a93b2b7dfdac77ceba5a558a580e74667dd6fede4585b91eefb60f03b72df23",
    strip_prefix = "zlib-1.3.1",
    urls = [
        "https://zlib.net/fossils/zlib-1.3.1.tar.gz",
    ],
)

# Load gazelle. This lets us auto-generate BUILD.bazel files throughout the
# repo.
http_archive(
    name = "bazel_gazelle",
    sha256 = "675114d8b433d0a9f54d81171833be96ebc4113115664b791e6f204d58e93446",
    urls = [
        "https://mirror.bazel.build/github.com/bazelbuild/bazel-gazelle/releases/download/v0.47.0/bazel-gazelle-v0.47.0.tar.gz",
        "https://github.com/bazelbuild/bazel-gazelle/releases/download/v0.47.0/bazel-gazelle-v0.47.0.tar.gz",
    ],
)

load("@bazel_gazelle//:deps.bzl", "gazelle_dependencies", "go_repository")

# Load up go dependencies (the ones listed under go.mod). The `DEPS.bzl` file
# is kept up to date using bazel run //:gazelle-update-repos
load("//:DEPS.bzl", "go_dependencies")

# googleapis is a dependency of grpcgateway.
http_archive(
    name = "googleapis",
    sha256 = "9d1a930e767c93c825398b8f8692eca3fe353b9aaadedfbcf1fca2282c85df88",
    strip_prefix = "googleapis-64926d52febbf298cb82a8f472ade4a3969ba922",
    urls = [
        "https://github.com/googleapis/googleapis/archive/64926d52febbf298cb82a8f472ade4a3969ba922.zip",
    ],
)

load("@googleapis//:repository_rules.bzl", "switched_rules_by_language")

switched_rules_by_language(
    name = "com_google_googleapis_imports",
)

http_archive(
    name = "com_github_sluongng_nogo_analyzer",
    sha256 = "58dfd32eb341282db63876db525246f3eb89c94f6a5a50252217620e2210c49a",
    strip_prefix = "nogo-analyzer-d6a3958307f2b485cda307f453e06db025415177",
    urls = [
        "https://github.com/sluongng/nogo-analyzer/archive/d6a3958307f2b485cda307f453e06db025415177.tar.gz",
    ],
)

# VERY IMPORTANT that we call into this function to prefer our pinned versions
# of the dependencies to any that might be pulled in via functions like
# `go_rules_dependencies`, `gazelle_dependencies`, `staticcheck_deps` etc.
# gazelle:repository_macro DEPS.bzl%go_dependencies
go_dependencies()

load("@com_github_sluongng_nogo_analyzer//staticcheck:deps.bzl", "staticcheck_deps")

staticcheck_deps()

############################################################
# Define your own dependencies here using go_repository.
# Else, dependencies declared by rules_go/gazelle will be used.
# The first declaration of an external repository "wins".
############################################################

load("@io_bazel_rules_go//go:deps.bzl", "go_register_toolchains", "go_rules_dependencies")

go_rules_dependencies()

go_register_toolchains(
    # nogo = "@//build/nogo:cc_nogo",
    # if you change this version, also change the tag & SHAs in the
    # container_pulls(...) for go-alpine below
    version = "1.26.10",
)

gazelle_dependencies()

#############################
# begin nodejs dependencies #
#############################
load("@bazel_tools//tools/build_defs/repo:http.bzl", "http_archive")

###########################
# end nodejs dependencies #
###########################

###############################
# begin protobuf dependencies #
###############################
# Load the protobuf dependency.
#
# Ref: https://github.com/bazelbuild/rules_go/tree/v0.34.0#protobuf-and-grpc
#      https://github.com/bazelbuild/bazel-gazelle/issues/591
#
#
#
#
http_archive(
    name = "rules_proto",
    sha256 = "6fb6767d1bef535310547e03247f7518b03487740c11b6c6adb7952033fe1295",
    strip_prefix = "rules_proto-6.0.2",
    url = "https://github.com/bazelbuild/rules_proto/releases/download/6.0.2/rules_proto-6.0.2.tar.gz",
)

http_archive(
    name = "com_google_protobuf",
    sha256 = "7c3ebd7aaedd86fa5dc479a0fda803f602caaf78d8aff7ce83b89e1b8ae7442a",
    strip_prefix = "protobuf-28.3",
    urls = [
        "https://github.com/protocolbuffers/protobuf/releases/download/v28.3/protobuf-28.3.tar.gz",
    ],
)

# Pin rules_python to a WORKSPACE-compatible version before protobuf_deps()
# pulls in a newer one that requires bzlmod-only rules_python_internal.
http_archive(
    name = "rules_python",
    sha256 = "5868e73107a8e85d8f323806e60cad7283f34b32163ea6ff1020cf27abef6036",
    strip_prefix = "rules_python-0.25.0",
    url = "https://github.com/bazelbuild/rules_python/releases/download/0.25.0/rules_python-0.25.0.tar.gz",
)

load("@com_google_protobuf//:protobuf_deps.bzl", "protobuf_deps")

protobuf_deps()

# rules_proto_grpc is nice helper library for generating protocol buffers. It
# was the easiest library to setup, provides compile targets for easy file
# hoisting, and was the easiest to extend as all our tooling is custom. If we
# can ever converge on mainline and up to date tools, we can consider other
# options.
http_archive(
    name = "rules_proto_grpc",
    sha256 = "fb7fc7a3c19a92b2f15ed7c4ffb2983e956625c1436f57a3430b897ba9864059",
    strip_prefix = "rules_proto_grpc-4.3.0",
    urls = ["https://github.com/rules-proto-grpc/rules_proto_grpc/archive/4.3.0.tar.gz"],
)

load("@rules_proto_grpc//:repositories.bzl", "rules_proto_grpc_repos", "rules_proto_grpc_toolchains")

rules_proto_grpc_toolchains()

rules_proto_grpc_repos()

load("@rules_proto//proto:toolchains.bzl", "rules_proto_toolchains")

load("@rules_proto//proto:repositories.bzl", "rules_proto_dependencies")

rules_proto_dependencies()

rules_proto_toolchains()

load("@rules_proto_grpc//:repositories.bzl", "bazel_gazelle", "io_bazel_rules_go")  # buildifier: disable=same-origin-load

io_bazel_rules_go()

bazel_gazelle()

load("@rules_proto_grpc//go:repositories.bzl", rules_proto_grpc_go_repos = "go_repos")

rules_proto_grpc_go_repos()

#############################
# end protobuf dependencies #
#############################

#############################
# begin docker dependencies #
#############################
http_archive(
    name = "rules_oci",
    sha256 = "cfea16076ebbec1faea494882ab97d94b1a62d6bcd5aceabad8f95ea0d0a1361",
    strip_prefix = "rules_oci-2.2.1",
    url = "https://github.com/bazel-contrib/rules_oci/releases/download/v2.2.1/rules_oci-v2.2.1.tar.gz",
)

http_archive(
    name = "rules_multirun",
    sha256 = "9b4dbc2d91fb135d888aad6c3582a13041a8a89c1efcd5956cc148f2a214f958",
    url = "https://github.com/keith/rules_multirun/releases/download/0.10.0/rules_multirun.0.10.0.tar.gz",
)

load("@rules_oci//oci:dependencies.bzl", "rules_oci_dependencies")

rules_oci_dependencies()

load("@rules_oci//oci:repositories.bzl", "oci_register_toolchains")

oci_register_toolchains(name = "oci")

load("@rules_oci//oci:pull.bzl", "oci_pull")

# We use a docker image mirror to avoid pulling from 3rd party repos, which sometimes have reliability issues.
oci_pull(
    name = "alpine",
    digest = "sha256:f71a5f071694a785e064f05fed657bf8277f1b2113a8ed70c90ad486d6ee54dc",
    image = "us-east1-docker.pkg.dev/crl-docker-sync/docker-io/library/alpine",
    platforms = [
        "linux/arm64/v8",
        "linux/amd64",
    ],
)

oci_pull(
    name = "distroless",
    # Pinned to a build carrying ca-certificates 20250419+deb13u1, which drops
    # the compromised eMudhra and Sectigo AAA roots. Do not move this digest
    # backwards.
    digest = "sha256:15f8637509792e05b97d83451b0b6ccfa4abd548f997e406ac1e72c87fe1fa78",
    image = "us-east1-docker.pkg.dev/crl-docker-sync/gcr-io/distroless/static",
    platforms = [
        "linux/arm64/v8",
        "linux/amd64",
    ],
)


load("@rules_oci//oci:pull.bzl", "oci_pull")

oci_pull(
    name = "intrusion-base",
    image = "us-docker.pkg.dev/cockroach-cloud-images/control-plane/intrusion-base",
    digest = "sha256:6482a29314569275da8ae1a584bff24615946a88c01cc05a420627b5c11df2f7",
    platforms = [
        "linux/amd64",
        "linux/arm64",
    ],
)

oci_pull(
    name = "go-alpine",
    # digest corresponds to tag 1.26.10-alpine
    digest = "sha256:a2012b4a1d1f9f7e9f44e0d39b805a901e856b1dd80b4e6ee4338db547050308",
    image = "us-east1-docker.pkg.dev/crl-docker-sync/docker-io/library/golang",
    platforms = [
        "linux/arm64/v8",
        "linux/amd64",
    ],
)

oci_pull(
    name = "explore-base",
    digest = "sha256:8e315f41a34fee06f2817d2bfcfff5e5b9455b7a6b555b10a484ffa693d2bfa4",
    image = "us-docker.pkg.dev/cockroach-cloud-images/data-plane/explore-base",
    platforms = [
        "linux/arm64",
        "linux/amd64",
    ],
)

oci_pull(
    name = "insitu-base",
    digest = "sha256:9f6ece603e4fb7c4a16b32e4e4d8d010c1f7b5ecf06b659dc04fe482287dac38",
    image = "us-docker.pkg.dev/cockroach-cloud-images/data-plane/in-situ-shell-base",
    platforms = [
        "linux/arm64",
        "linux/amd64",
    ],
)

oci_pull(
    name = "inotifywait-base-distroless",
    # tag = "4c37f94d89046e6dd900ea3f3dde5051581edc81"
    digest = "sha256:c2c9ed2d2640d929d2670aa457af042c0242a8a54b73f47daa8c6afc7e644ba0",
    image = "us-docker.pkg.dev/cockroach-cloud-images/data-plane/inotifywait-base",
    platforms = [
        "linux/arm64",
        "linux/amd64",
    ],
)

oci_pull(
    name = "inotifywait-base-ubi",
    # tag = "v2.0.1"
    digest = "sha256:b67659fe7f8ae9107679544c61213fb1d23be6572a1855fe9cb3de37c15a6915",
    # UBI10-minimal base image with inotify-tools and procps-ng (multi-arch),
    # rebuilt from services/inotifywait/Dockerfile. The v2.0.1 rebuild picks up
    # a ca-certificates that no longer trusts the compromised roots, so do not
    # move this digest backwards.
    image = "us-docker.pkg.dev/releases-prod/self-hosted/inotifywait-base",
    platforms = [
        "linux/arm64",
        "linux/amd64",
    ],
)

oci_pull(
    name = "ubi-minimal",
    digest = "sha256:ab67cd02caf8706e34a695a1432ebe965934f6128ec352a6c3f4048b43a4fb5e",
    # Red Hat UBI10 minimal base image. Bumped from UBI9 to fix a D health
    # grade on the init-container in Red Hat Partner Connect (CC-35894).
    # Refresh this digest when Red Hat publishes new patch releases. The
    # current pin also drops the compromised eMudhra roots, so do not move it
    # backwards.
    image = "registry.access.redhat.com/ubi10/ubi-minimal",
    platforms = [
        "linux/arm64/v8",
        "linux/amd64",
    ],
)

oci_pull(
    name = "ubi-micro",
    digest = "sha256:37fadb004c6bea628fcdd81376c8fb77bd8d9fd432d90503af4d9e76b1ff7191",
    # Red Hat UBI10 micro base image (even smaller than minimal).
    image = "registry.access.redhat.com/ubi10/ubi-micro",
    platforms = [
        "linux/arm64/v8",
        "linux/amd64",
    ],
)

#############################
# end docker dependencies #
#############################

#############################
# start pulumi dependencies #
#############################

# NOTE: The versions of Pulumi and it's plugins are pulled from
# Makefile-pulumi. Any changes made to that file will need to be
# reflected here.

http_archive(
    name = "pulumi-darwin",
    build_file_content = '''
exports_files(["pulumi", "pulumi-language-go"])
''',
    sha256 = "bcf301f7e107365355f683743cf54d12fca6ff7513c4c153e312965e55fec632",
    strip_prefix = "pulumi",
    url = "https://get.pulumi.com/releases/sdk/pulumi-v3.17.1-darwin-x64.tar.gz",
)

http_archive(
    name = "pulumi-linux",
    build_file_content = '''
exports_files(["pulumi", "pulumi-language-go"])
''',
    sha256 = "2ed71932133a57d920e8e95341bbc961d7206eedddbd9e638bd4330eb62cf5da",
    strip_prefix = "pulumi",
    url = "https://get.pulumi.com/releases/sdk/pulumi-v3.17.1-linux-x64.tar.gz",
)

http_archive(
    name = "pulumi-gcp-darwin",
    build_file_content = '''
exports_files(["pulumi-resource-gcp"])
''',
    sha256 = "fa9640d68832f622bd524dc8e8344cc308bf027085b2efe597bb8448a0e806b8",
    url = "https://github.com/pulumi/pulumi-gcp/releases/download/v6.65.1/pulumi-resource-gcp-v6.65.1-darwin-amd64.tar.gz",
)

http_archive(
    name = "pulumi-gcp-linux",
    build_file_content = '''
exports_files(["pulumi-resource-gcp"])
''',
    sha256 = "88110e8d324864926e6b4fff1b155eacec0ff75779563d6637448a8edbb8ad1c",
    url = "https://github.com/pulumi/pulumi-gcp/releases/download/v6.65.1/pulumi-resource-gcp-v6.65.1-linux-amd64.tar.gz",
)

http_archive(
    name = "pulumi-aws-darwin",
    build_file_content = '''
exports_files(["pulumi-resource-aws"])
''',
    sha256 = "8371436c8e09685ae19277546e5fad96d40fc933b99c5fa91648f97410e6234e",
    url = "https://github.com/pulumi/pulumi-aws/releases/download/v6.5.0/pulumi-resource-aws-v6.5.0-darwin-amd64.tar.gz",
)

http_archive(
    name = "pulumi-aws-linux",
    build_file_content = '''
exports_files(["pulumi-resource-aws"])
''',
    sha256 = "d8184e778570f1b3def38cc85d58f7155f7fd6cde032bd6115241bed6476eb9c",
    url = "https://github.com/pulumi/pulumi-aws/releases/download/v6.5.0/pulumi-resource-aws-v6.5.0-linux-amd64.tar.gz",
)

http_archive(
    name = "pulumi-azuread-darwin",
    build_file_content = '''
exports_files(["pulumi-resource-azuread"])
''',
    sha256 = "47a9abbc7fe9b41d677d8bda56cbaf59ecc13822b83ff30b6341136334d8dfe3",
    url = "https://github.com/pulumi/pulumi-azuread/releases/download/v5.38.0/pulumi-resource-azuread-v5.38.0-darwin-amd64.tar.gz",
)

http_archive(
    name = "pulumi-azuread-linux",
    build_file_content = '''
exports_files(["pulumi-resource-azuread"])
''',
    sha256 = "bd4d8dc0e34d9978e9058627cda2fb0fc9c8175553a12a5f4db06157fdfa5348",
    url = "https://github.com/pulumi/pulumi-azuread/releases/download/v5.38.0/pulumi-resource-azuread-v5.38.0-linux-amd64.tar.gz",
)

http_archive(
    name = "pulumi-azure-native-darwin",
    build_file_content = '''
exports_files(["pulumi-resource-azure-native"])
''',
    sha256 = "7cbea57d3024ff4ae44fa6512922e6d3e20028ddab2761262e1c84540c1878a5",
    url = "https://github.com/pulumi/pulumi-azure-native/releases/download/v2.3.0/pulumi-resource-azure-native-v2.3.0-darwin-amd64.tar.gz",
)

http_archive(
    name = "pulumi-azure-native-linux",
    build_file_content = '''
exports_files(["pulumi-resource-azure-native"])
''',
    sha256 = "7e97c6976d7eedac6b5fac7c9243dc14540f59a967c84d49c197d05804f9b8ad",
    url = "https://github.com/pulumi/pulumi-azure-native/releases/download/v2.3.0/pulumi-resource-azure-native-v2.3.0-linux-amd64.tar.gz",
)

http_archive(
    name = "pulumi-kubernetes-darwin",
    build_file_content = '''
exports_files(["pulumi-resource-kubernetes"])
''',
    sha256 = "4d78fed842aadeceb47f0fa9392f84c065c8b5f8165b2f301483d73ab7cb6a16",
    url = "https://github.com/pulumi/pulumi-kubernetes/releases/download/v3.10.0/pulumi-resource-kubernetes-v3.10.0-darwin-amd64.tar.gz",
)

http_archive(
    name = "pulumi-kubernetes-linux",
    build_file_content = '''
exports_files(["pulumi-resource-kubernetes"])
''',
    sha256 = "58b9f156bb0fb193825c55bb945c7d2f5fe1489a77541441278e96cbbdc8c7bb",
    url = "https://github.com/pulumi/pulumi-kubernetes/releases/download/v3.10.0/pulumi-resource-kubernetes-v3.10.0-linux-amd64.tar.gz",
)
###########################
# end pulumi dependencies #
###########################

####################################
# begin bazel utility dependencies #
####################################
load("@bazel_tools//tools/build_defs/repo:http.bzl", "http_archive")

http_archive(
    name = "aspect_bazel_lib",
    sha256 = "4d6010ca5e3bb4d7045b071205afa8db06ec11eb24de3f023d74d77cca765f66",
    strip_prefix = "bazel-lib-1.39.0",
    url = "https://github.com/aspect-build/bazel-lib/releases/download/v1.39.0/bazel-lib-v1.39.0.tar.gz",
)

load("@aspect_bazel_lib//lib:repositories.bzl", "aspect_bazel_lib_dependencies", "aspect_bazel_lib_register_toolchains")

aspect_bazel_lib_dependencies()

aspect_bazel_lib_register_toolchains()

# jq is a lightweight and flexible command-line JSON processor.
# register_jq_toolchains() installs JQ and for use in the jq rule.
load("@aspect_bazel_lib//lib:repositories.bzl", "register_jq_toolchains")

register_jq_toolchains()
####################################
#  end bazel utility dependencies  #
####################################

# We vendor the directory.proto and license.proto files from CRDB so we don't
# have to inherit its nasty import graph and we can eventually avoid a dep on
# CRDB entirely.
http_file(
    name = "tenantdir",
    downloaded_file_path = "directory.proto",
    sha256 = "e333188f4c5be0b88a32358fda67522669fc02140d075b59f127182be6c14cb8",
    url = "https://raw.githubusercontent.com/cockroachdb/cockroach/b6f9e62b9940da5989f91b25019fb4574924163b/pkg/ccl/sqlproxyccl/tenant/directory.proto",
)
