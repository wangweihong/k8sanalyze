package(default_visibility = ["//visibility:public"])

licenses(["notice"])

load(
    "@io_bazel_rules_go//go:def.bzl",
    "go_library",
    "go_test",
)

go_library(
    name = "go_default_library",
    srcs = [
        "policy_comparator.go",
        "rulevalidation.go",
        "validation.go",
    ],
    tags = ["automanaged"],
    deps = [
        "//pkg/api/errors:go_default_library",
        "//pkg/api/validation:go_default_library",
        "//pkg/api/validation/path:go_default_library",
        "//pkg/apis/rbac:go_default_library",
        "//pkg/genericapiserver/api/request:go_default_library",
        "//pkg/serviceaccount:go_default_library",
        "//pkg/util/errors:go_default_library",
        "//pkg/util/validation/field:go_default_library",
        "//vendor:github.com/golang/glog",
        "//vendor:k8s.io/apiserver/pkg/authentication/user",
    ],
)

go_test(
    name = "go_default_test",
    srcs = [
        "policy_comparator_test.go",
        "rulevalidation_test.go",
        "validation_test.go",
    ],
    library = ":go_default_library",
    tags = ["automanaged"],
    deps = [
        "//pkg/api:go_default_library",
        "//pkg/apis/rbac:go_default_library",
        "//pkg/util/diff:go_default_library",
        "//pkg/util/validation/field:go_default_library",
        "//vendor:k8s.io/apiserver/pkg/authentication/user",
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
