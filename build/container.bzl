load("@rules_oci//oci:defs.bzl", "oci_image", "oci_image_index", _oci_push = "oci_push")
load("@aspect_bazel_lib//lib:transitions.bzl", "platform_transition_filegroup")
load("@rules_pkg//pkg:tar.bzl", "pkg_tar")
load("@rules_multirun//:defs.bzl", "command", "multirun")
load("@aspect_bazel_lib//lib:expand_template.bzl", "expand_template")

oci_push = _oci_push

# control_plane_image constructs a single-platform image using rules_oci.
#
# The platform is specifically linux/amd64, so this should only be used for images
# that run in the control plane ONLY; anything which runs in the data plane should
# use data_plane_image (since we will soon run dataplanes on x86 and ARM)
def control_plane_image(
    name,
    directory,
    files = None,
    empty_files = None,
    **kwargs,
):
    file_structure = {directory: {}}
    if files != None:
        file_structure[directory]["files"] = files
    if empty_files != None:
        file_structure[directory]["empty_files"] = empty_files

    oci_go_image(
        name = name,
        file_structure = file_structure,
        platform_amd64 = True,
        platform_arm64 = False,
        **kwargs,
    )

# data_plane_image constructs a multi-platform image using rules_oci.
#
# If you need add multiple files in different directories, use the
# oci_go_image rule.
def data_plane_image(
    name,
    directory,
    files = None,
    empty_files = None,
    labels = None,  # Add this parameter
    **kwargs,
):
    file_structure = {directory: {}}
    if files != None:
        file_structure[directory]["files"] = files
    if empty_files != None:
        file_structure[directory]["empty_files"] = empty_files

    oci_go_image(
        name = name,
        file_structure = file_structure,
        platform_amd64 = True,
        platform_arm64 = True,
        labels = labels,  # Pass labels through
        **kwargs,
    )

# oci_go_image constructs a multi-platform image,
# adding files to multiple different directories.
#
# The file_structure param expects a dictionary of directories to file
# configs. Each file config is a dict containing two optional lists:
# files and empty_files.
#
# Example:
# {
#     "/etc": {
#         "files": [
#             "config.yaml",
#         ],
#         "empty_files": [
#             "template.yaml",
#         ],
#     },
#     "/bin": {
#         "files": [
#             "run.sh",
#         ]
#     }
# }
def oci_go_image(
    name,
    file_structure = None,
    platform_amd64 = True,
    platform_arm64 = True,
    labels = None,  # Add this parameter
    **kwargs,
):
    # Pass labels to oci_image if provided
    image_kwargs = dict(kwargs)
    if labels != None:
        image_kwargs["labels"] = labels

    # Add LICENSE file layer for Red Hat certification (UBI builds only)
    # The license layer is conditionally added based on the use_ubi flag
    num_layers = len(file_structure)

    # Conditionally include LICENSE layer for UBI builds
    oci_image(
        name = "%s_base" % name,
        tars = ["layer_%d" % index for index in range(0, num_layers)] + select({
            "//build:use_ubi": ["//:license_layer"],
            "//conditions:default": [],
        }),
        **image_kwargs,
    )

    [
        pkg_tar(
            name = "layer_%d" % index,
            package_dir = directory,
            srcs = file_structure.get(directory).get("files"),
            empty_files = file_structure.get(directory).get("empty_files"),
            extension = "tar.gz",
        )
        for index, directory in enumerate(file_structure)
    ]

    images = []
    if platform_arm64:
        platform_transition_filegroup(
            name = "%s_arm64" % name,
            srcs = ["%s_base" % name],
            target_platform = "@io_bazel_rules_go//go/toolchain:linux_arm64",
        )
        images.append("%s_arm64" % name)

    if platform_amd64:
        platform_transition_filegroup(
            name = "%s_amd64" % name,
            srcs = ["%s_base" % name],
            target_platform = "@io_bazel_rules_go//go/toolchain:linux_amd64",
        )
        images.append("%s_amd64" % name)

    oci_image_index(
        name = name,
        images = images,
        visibility = ["//visibility:public"],
    )

def oci_push_all(name, image_config, stamped_tags = [], static_tags = [], jobs = 1):
    [
        oci_push_with_tags(
            name = _push_name(name, repository),
            image = image,
            repository = repository,
            stamped_tags = stamped_tags,
            static_tags = static_tags,
        )
        for repository, image in image_config.items()
    ]

    [
        command(
            name = "%s_command" % _push_name(name, repository),
            command = _push_name(name, repository),
        )
        for repository, image in image_config.items()
    ]

    command_names = [("%s_command" % _push_name(name, repository)) for repository, image in image_config.items()]

    multirun(
        name = name,
        commands = command_names,
        jobs = jobs,
    )

# oci_push_with_tags is a helper function for pushing an image.
#
# static_tags is a list of tags that will be directly applied to the pushed image.
#
# stamped_tags is a list of strings that correspond to information
# stamped workspace information. See build/stamp.sh for the available
# options.
def oci_push_with_tags(name, stamped_tags = [], static_tags = [], **kwargs):
    expand_template(
        name = "%s_build_tags" % name,
        template = stamped_tags + static_tags,
        substitutions = dict({
            tag: "{{%s}}" % tag
            for tag in stamped_tags
        }, **{
            tag: tag
            for tag in static_tags
        })
    )

    oci_push(
        name = name,
        remote_tags = "%s_build_tags" % name,
        **kwargs,
    )

def _push_name(name, repo):
    return (name + "_" + repo).replace(":", "")
