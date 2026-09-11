package buildinfo

import _ "embed"

// Licenses retains attribution in every distributed binary, including updates.
//
//go:embed notices.txt
var Licenses string
