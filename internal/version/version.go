package version

const (
	Major = "3"
	Minor = "80"
	Patch = "5"

	Package = "ydb-go-sdk"
)

const (
	Version     = Major + "." + Minor + "." + Patch
	FullVersion = Package + "/" + Version
)
