package(default_visibility = ["//visibility:public"])

licenses(["notice"])

load(
    "@io_bazel_rules_go//go:def.bzl",
    "go_library",
)

go_library(
    name = "go_default_library",
    srcs = [
        "apiservice.go",
        "interface.go",
    ],
    tags = ["automanaged"],
    deps = [
        "//cmd/kubernetes-discovery/pkg/apis/apiregistration:go_default_library",
        "//cmd/kubernetes-discovery/pkg/client/clientset_generated/internalclientset:go_default_library",
        "//cmd/kubernetes-discovery/pkg/client/informers/internalinterfaces:go_default_library",
        "//cmd/kubernetes-discovery/pkg/client/listers/apiregistration/internalversion:go_default_library",
        "//pkg/api:go_default_library",
        "//pkg/api/v1:go_default_library",
        "//pkg/client/cache:go_default_library",
        "//pkg/runtime:go_default_library",
        "//pkg/watch:go_default_library",
    ],
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
