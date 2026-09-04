//go:build tools

// Package tools pins the aggregated-apiserver dependency tree in go.mod so the
// versions stay stable across `go mod tidy` before the server code (which
// actually imports these packages) lands in a later phase. The `tools` build
// tag keeps these blank imports out of every normal build while still being
// visible to `go mod tidy`, which resolves imports across all build tags.
package tools

import (
	_ "k8s.io/api/core/v1"
	_ "k8s.io/apiserver/pkg/server"
	_ "k8s.io/component-base/version"
	_ "k8s.io/kube-openapi/pkg/common"
)
