package(default_visibility = ["//visibility:public"])

load(
    "@io_bazel_rules_go//go:def.bzl",
    "go_library",
)

go_library(
    name = "go_default_library",
    srcs = [
        "flags.go",
        "flags_unsupported.go",
        "mount.go",
        "mounter_unsupported.go",
        "mountinfo.go",
        "mountinfo_unsupported.go",
    ] + select({
        "@io_bazel_rules_go//go/platform:linux_amd64": [
            "flags_linux.go",
            "mounter_linux.go",
            "mountinfo_linux.go",
            "sharedsubtree_linux.go",
        ],
        "@io_bazel_rules_go//go/platform:windows_amd64": [
            "mountinfo_windows.go",
        ],
        "//conditions:default": [],
    }),
    cgo = True,
)

filegroup(
    name = "package-srcs",
    srcs = glob(["**"]),
    tags = ["automanaged"],
    visibility = ["//visibility:private"],
)

filegroup(
    name = "all-srcs",
    srcs = [":package-srcs"],
    tags = ["automanaged"],
)
