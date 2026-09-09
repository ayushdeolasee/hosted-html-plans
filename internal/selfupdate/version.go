package selfupdate

import (
	"fmt"
	"strconv"
	"strings"
)

type stableVersion struct {
	major uint64
	minor uint64
	patch uint64
}

func parseStableVersion(raw string) (stableVersion, error) {
	v := strings.TrimSpace(raw)
	if strings.HasPrefix(v, "v") {
		v = v[1:]
	}
	if v == "" || strings.Contains(v, "-") {
		return stableVersion{}, fmt.Errorf("%q is not a stable semantic version", raw)
	}
	if before, metadata, ok := strings.Cut(v, "+"); ok {
		if !validSemverIdentifiers(metadata) {
			return stableVersion{}, fmt.Errorf("%q has invalid semantic-version build metadata", raw)
		}
		v = before
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return stableVersion{}, fmt.Errorf("%q is not a semantic version", raw)
	}
	values := make([]uint64, 3)
	for i, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return stableVersion{}, fmt.Errorf("%q is not a semantic version", raw)
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return stableVersion{}, fmt.Errorf("%q is not a semantic version", raw)
			}
		}
		n, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return stableVersion{}, fmt.Errorf("%q is not a semantic version: %w", raw, err)
		}
		values[i] = n
	}
	return stableVersion{major: values[0], minor: values[1], patch: values[2]}, nil
}

func validSemverIdentifiers(value string) bool {
	if value == "" {
		return false
	}
	for _, identifier := range strings.Split(value, ".") {
		if identifier == "" {
			return false
		}
		for _, r := range identifier {
			if (r < '0' || r > '9') && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && r != '-' {
				return false
			}
		}
	}
	return true
}

func compareVersions(a, b stableVersion) int {
	av := [...]uint64{a.major, a.minor, a.patch}
	bv := [...]uint64{b.major, b.minor, b.patch}
	for i := range av {
		if av[i] < bv[i] {
			return -1
		}
		if av[i] > bv[i] {
			return 1
		}
	}
	return 0
}
