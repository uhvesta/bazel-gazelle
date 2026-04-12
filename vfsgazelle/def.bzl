load("@io_bazel_rules_go//go:def.bzl", "GoArchive", "go_context", "new_go_info")

DEFAULT_LANGUAGES = [
    Label("//vfsgazelle/language/proto"),
    Label("//vfsgazelle/language/go"),
]

def _import_alias(importpath):
    return importpath.replace("/", "_").replace(".", "_").replace("-", "_") + "_"

def _format_import(importpath):
    return '{} "{}"'.format(_import_alias(importpath), importpath)

def _format_call(importpath):
    return _import_alias(importpath) + ".NewLanguage()"

def _gazelle_vfsgazelle_binary_impl(ctx):
    go = go_context(ctx)

    langs_file = go.declare_file(go, "langs.go")
    langs_content = """
package main

import (
\tvfsgazellelanguage "github.com/uhvesta/bazel-gazelle/vfsgazelle/language"

\t{lang_imports}
)

var languages = []vfsgazellelanguage.Language{{
\t{lang_calls},
}}
""".format(
        lang_imports = "\n\t".join([_format_import(d[GoArchive].data.importpath) for d in ctx.attr.languages]),
        lang_calls = ",\n\t".join([_format_call(d[GoArchive].data.importpath) for d in ctx.attr.languages]),
    )
    go.actions.write(langs_file, langs_content)

    attr = struct(
        srcs = [struct(files = [langs_file])],
        deps = ctx.attr.languages,
        embed = [ctx.attr._srcs],
    )
    go_info = new_go_info(go, attr, is_main = True)

    archive, executable, runfiles = go.binary(
        go,
        name = ctx.label.name,
        source = go_info,
        version_file = ctx.version_file,
        info_file = ctx.info_file,
    )

    return [
        go_info,
        archive,
        OutputGroupInfo(compilation_outputs = [archive.data.file]),
        DefaultInfo(
            files = depset([executable]),
            runfiles = runfiles,
            executable = executable,
        ),
    ]

gazelle_vfsgazelle_binary = rule(
    implementation = _gazelle_vfsgazelle_binary_impl,
    attrs = {
        "languages": attr.label_list(
            providers = [GoArchive],
            mandatory = True,
            allow_empty = False,
            doc = "List of vfsgazelle language libraries exporting NewLanguage().",
        ),
        "_go_context_data": attr.label(default = "@io_bazel_rules_go//:go_context_data"),
        "_stdlib": attr.label(default = "@io_bazel_rules_go//:stdlib"),
        "_srcs": attr.label(default = "//vfsgazelle/cmd/gazelle:gazelle_lib"),
    },
    executable = True,
    toolchains = ["@io_bazel_rules_go//go:toolchain"],
)
