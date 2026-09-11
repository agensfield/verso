package updater

import (
	"strconv"
	"strings"
)

type version struct {
	major, minor, patch int
	pre                 []string
}

func parseVersion(raw string) (version, bool) {
	raw = strings.TrimPrefix(strings.TrimSpace(raw), "v")
	base, pre, _ := strings.Cut(raw, "-")
	parts := strings.Split(base, ".")
	if len(parts) != 3 {
		return version{}, false
	}
	values := make([]int, 3)
	for i, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return version{}, false
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return version{}, false
		}
		values[i] = n
	}
	v := version{major: values[0], minor: values[1], patch: values[2]}
	if pre != "" {
		v.pre = strings.Split(pre, ".")
		for _, identifier := range v.pre {
			if identifier == "" || strings.ContainsFunc(identifier, func(r rune) bool {
				return !(r >= '0' && r <= '9') && !(r >= 'A' && r <= 'Z') && !(r >= 'a' && r <= 'z') && r != '-'
			}) {
				return version{}, false
			}
			if len(identifier) > 1 && identifier[0] == '0' && strings.IndexFunc(identifier, func(r rune) bool { return r < '0' || r > '9' }) == -1 {
				return version{}, false
			}
		}
	}
	return v, true
}

func (v version) compare(other version) int {
	for _, pair := range [][2]int{{v.major, other.major}, {v.minor, other.minor}, {v.patch, other.patch}} {
		if pair[0] < pair[1] {
			return -1
		}
		if pair[0] > pair[1] {
			return 1
		}
	}
	if len(v.pre) == 0 && len(other.pre) != 0 {
		return 1
	}
	if len(v.pre) != 0 && len(other.pre) == 0 {
		return -1
	}
	for i := 0; i < len(v.pre) && i < len(other.pre); i++ {
		a, aErr := strconv.Atoi(v.pre[i])
		b, bErr := strconv.Atoi(other.pre[i])
		switch {
		case aErr == nil && bErr == nil && a < b:
			return -1
		case aErr == nil && bErr == nil && a > b:
			return 1
		case aErr == nil && bErr != nil:
			return -1
		case aErr != nil && bErr == nil:
			return 1
		case v.pre[i] < other.pre[i]:
			return -1
		case v.pre[i] > other.pre[i]:
			return 1
		}
	}
	if len(v.pre) < len(other.pre) {
		return -1
	}
	if len(v.pre) > len(other.pre) {
		return 1
	}
	return 0
}

func compareVersions(current, latest string) Status {
	a, aOK := parseVersion(current)
	b, bOK := parseVersion(latest)
	if !aOK || !bOK {
		return StatusUnknown
	}
	switch comparison := a.compare(b); {
	case comparison < 0:
		return StatusOutdated
	case comparison > 0:
		return StatusAhead
	default:
		return StatusCurrent
	}
}
