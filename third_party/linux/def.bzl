#  Copyright 2020 The Monogon Project Authors.
#
#  SPDX-License-Identifier: Apache-2.0
#
#  Licensed under the Apache License, Version 2.0 (the "License");
#  you may not use this file except in compliance with the License.
#  You may obtain a copy of the License at
#
#      http://www.apache.org/licenses/LICENSE-2.0
#
#  Unless required by applicable law or agreed to in writing, software
#  distributed under the License is distributed on an "AS IS" BASIS,
#  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
#  See the License for the specific language governing permissions and
#  limitations under the License.

"""
Rules for building Linux kernel images.

This currently performs the build in a fully unhermetic manner, using
make/gcc/... from the host, and is only slightly better than a genrule. This
should be replaced by a hermetic build that at least uses rules_cc toolchain
information, or even better, just uses cc_library targets.
"""

load("//build/utils:detect_root.bzl", "detect_root")


def _ignore_unused_configuration_impl(settings, attr):
    return {
        # This list should be expanded with any configuration options that end
        # up reaching this rule with different values across different build
        # graph paths, but that do not actually influence the kernel build.
        # Force-setting them to a stable value forces the build configuration
        # to a stable hash.
        # See the transition's comment block for more information.
        "@io_bazel_rules_go//go/config:pure": True,
        "@io_bazel_rules_go//go/config:static": True,
        # Note: this toolchain is not actually used to perform the build.
        "//command_line_option:crosstool_top": "//build/toolchain/musl-host-gcc:musl_host_cc_suite",
    }

# Transition to flip all known-unimportant but varying configuration options to
# a known, stable value.
# This is to prevent Bazel from creating extra configurations for possible
# combinations of options in case the linux_image rule is pulled through build
# graph fragments that have different options set.
#
# Ideally, Bazel would let us mark in a list that we only care about some set
# of options (or at least let us mark those that we explicitly don't care
# about, instead of manually setting them to some value). However, this doesn't
# seem to be possible, thus this transition is a bit of a hack.
ignore_unused_configuration = transition(
    implementation = _ignore_unused_configuration_impl,
    inputs = [],
    outputs = [
        "@io_bazel_rules_go//go/config:pure",
        "@io_bazel_rules_go//go/config:static",
        "//command_line_option:crosstool_top",
    ],
)


def _linux_image_impl(ctx):
    kernel_config = ctx.file.kernel_config
    kernel_src = ctx.files.kernel_src
    image_format = ctx.attr.image_format

    # Tuple containing information about how to build and access the resulting
    # image.
    # The first element (target) is the make target to build, the second
    # (output_source) is the resulting file to be copied and the last
    # (output_name) is the name of the output that will be generated by this
    # rule.
    (target, output_source, output_name) = {
        'vmlinux': ('vmlinux', 'vmlinux', 'vmlinux'),
        'bzImage': ('all', 'arch/x86/boot/bzImage', 'bzImage'),
    }[image_format]

    # Root of the given Linux sources.
    root = detect_root(ctx.attr.kernel_src)

    output = ctx.actions.declare_file(output_name)
    ctx.actions.run_shell(
        outputs = [ output ],
        inputs = [ kernel_config ] + kernel_src,
        command = '''
            kconfig=$1
            target=$2
            output_source=$3
            output=$4
            root=$5

            mkdir ${root}/.bin
            cp ${kconfig} ${root}/.config
            (cd ${root} && make -j $(nproc) ${target} >/dev/null)
            cp ${root}/${output_source} ${output}
        ''',
        arguments = [
            kernel_config.path,
            target,
            output_source,
            output.path,
            root,
        ],
        use_default_shell_env = True,
    )

    files = depset([output])
    runfiles = ctx.runfiles(files=[output])
    return [DefaultInfo(files=files, runfiles=runfiles)]


linux_image = rule(
    doc = '''
        Build Linux kernel image unhermetically in a given format.
    ''',
    implementation = _linux_image_impl,
    cfg = ignore_unused_configuration,
    attrs = {
        "kernel_config": attr.label(
            doc = '''
                Linux kernel configuration file to build this kernel image with.
            ''',
            allow_single_file = True,
            default = ":linux-metropolis.config",
        ),
        "kernel_src": attr.label(
            doc = '''
                Filegroup containing Linux kernel sources.
            ''',
            default = "@linux//:all",
        ),
        "image_format": attr.string(
            doc = '''
                Format of generated Linux image, one of 'vmlinux' or 'bzImage',
            ''',
            values = [
                'vmlinux', 'bzImage',
            ],
            default = 'bzImage',
        ),
        "_allowlist_function_transition": attr.label(
            default = "@bazel_tools//tools/allowlists/function_transition_allowlist"
        ),
    },
)