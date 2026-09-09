module github.com/ChronicCmposer/gitd

go 1.26.3

require (
	github.com/aws/aws-sdk-go-v2 v1.46.0
	github.com/aws/aws-sdk-go-v2/config v1.33.3
	github.com/aws/aws-sdk-go-v2/service/s3 v1.112.0
	// jonbaldie/go-mutesting/v2 is the Phase 9 mutation-tool pin (R4-Q1).
	// Consumed by the rules_go-built mutation binary //tools/mutesting
	// (Bazel-only; no Go source imports it, so `go mod tidy` would drop it —
	// do not run tidy on this repo).
	github.com/jonbaldie/go-mutesting/v2 v2.7.9
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/alecthomas/chroma/v2 v2.27.0
	github.com/yuin/goldmark v1.8.6
	github.com/yuin/goldmark-highlighting/v2 v2.0.0-20230729083705-37449abec8cc
)

require (
	github.com/avast/retry-go v3.0.0+incompatible // indirect
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.7.20 // indirect
	github.com/aws/aws-sdk-go-v2/credentials v1.20.3 // indirect
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.19.2 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.5.2 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.8.2 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.5.2 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.19 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/checksum v1.11.2 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.14.2 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/s3shared v1.20.2 // indirect
	github.com/aws/aws-sdk-go-v2/service/signin v1.9.0 // indirect
	github.com/aws/aws-sdk-go-v2/service/sso v1.37.0 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssooidc v1.42.0 // indirect
	github.com/aws/aws-sdk-go-v2/service/sts v1.49.0 // indirect
	github.com/aws/smithy-go v1.28.1 // indirect
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/dlclark/regexp2/v2 v2.2.1 // indirect
	github.com/fatih/color v1.18.0 // indirect
	github.com/jessevdk/go-flags v1.5.0 // indirect
	github.com/mattn/go-colorable v0.1.13 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/mitchellh/colorstring v0.0.0-20190213212951-d06e56a500db // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/schollz/progressbar/v3 v3.14.2 // indirect
	github.com/stretchr/testify v1.9.0 // indirect
	github.com/termie/go-shutil v0.0.0-20140729215957-bcacb06fecae // indirect
	github.com/ulikunitz/xz v0.5.15 // indirect
	github.com/zimmski/osutil v1.6.1 // indirect
	golang.org/x/mod v0.24.0 // indirect
	golang.org/x/sync v0.12.0 // indirect
	golang.org/x/sys v0.31.0 // indirect
	golang.org/x/term v0.17.0 // indirect
	golang.org/x/tools v0.31.0 // indirect
	gopkg.in/check.v1 v1.0.0-20201130134442-10cb98267c6c // indirect
)
