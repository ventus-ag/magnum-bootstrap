package buildinfo

import (
	"fmt"
	"strings"
)

var Version = "dev"

var Repository = "ventus-ag/magnum-bootstrap"

const HostProviderAsset = "pulumi-resource-magnumhost"

func IsTaggedRelease() bool {
	if len(Version) <= 1 || Version[0] != 'v' {
		return false
	}
	if strings.Contains(Version, "-dirty") || strings.Contains(Version, "-g") {
		return false
	}
	return true
}

func ReleaseAssetURL(asset string) string {
	if !IsTaggedRelease() || Repository == "" || asset == "" {
		return ""
	}
	return fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", Repository, Version, asset)
}

func DefaultHostProviderURL() string {
	return ReleaseAssetURL(HostProviderAsset)
}
