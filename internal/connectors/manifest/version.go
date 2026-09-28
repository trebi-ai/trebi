package manifest

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
)

// Version is a semver 2.0 version. Build metadata is kept for display and
// ignored in the order, so 1.3.0+2 sorts equal to 1.3.0.
type Version struct {
	Major, Minor, Patch int
	Pre                 string
	Build               string
}

// Range is a set of comparator groups: any group matches when all of its
// comparators match ("||" separates groups, space joins comparators).
type Range struct {
	groups [][]comparator
	text   string
}

type comparator struct {
	op string
	v  Version
}

// Track selects which newer versions an entry follows.
type Track string

// Tracks of Part C.1.
const (
	TrackExact  Track = "exact"
	TrackPatch  Track = "patch"
	TrackMinor  Track = "minor"
	TrackLatest Track = "latest"
)

// compiledSchema resolves the embedded JSON Schema once.
var compiledSchema = sync.OnceValues(func() (*jsonschema.Resolved, error) { //nolint:gochecknoglobals // immutable compiled form of the embedded schema
	var s jsonschema.Schema
	if err := json.Unmarshal(schemaJSON, &s); err != nil {
		return nil, err
	}
	return s.Resolve(nil)
})

// ParseVersion reads "1.2.3", "1.2.3-rc.1", or "1.2.3+2". A leading "v" is
// accepted.
func ParseVersion(s string) (Version, error) {
	raw := strings.TrimPrefix(strings.TrimSpace(s), "v")
	var v Version
	if raw == "" {
		return v, fmt.Errorf("version %q is not semver", s)
	}
	raw, v.Build, _ = strings.Cut(raw, "+")
	raw, v.Pre, _ = strings.Cut(raw, "-")
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("version %q is not semver", s)
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || (len(p) > 1 && p[0] == '0') {
			return Version{}, fmt.Errorf("version %q is not semver", s)
		}
		switch i {
		case 0:
			v.Major = n
		case 1:
			v.Minor = n
		default:
			v.Patch = n
		}
	}
	return v, nil
}

// MustVersion panics on a bad version. Tests use it.
func MustVersion(s string) Version {
	v, err := ParseVersion(s)
	if err != nil {
		panic(err)
	}
	return v
}

func (v Version) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.Pre != "" {
		s += "-" + v.Pre
	}
	if v.Build != "" {
		s += "+" + v.Build
	}
	return s
}

// Compare returns -1, 0, or 1. Build metadata does not count.
func (v Version) Compare(o Version) int {
	for _, d := range []int{v.Major - o.Major, v.Minor - o.Minor, v.Patch - o.Patch} {
		if d != 0 {
			return sign(d)
		}
	}
	switch {
	case v.Pre == o.Pre:
		return 0
	case v.Pre == "":
		return 1
	case o.Pre == "":
		return -1
	}
	return comparePre(v.Pre, o.Pre)
}

// CompareFull is Compare with build metadata as a last tie breaker: a
// manifest change (1.3.0+2) sorts after its base (1.3.0).
func (v Version) CompareFull(o Version) int {
	if c := v.Compare(o); c != 0 {
		return c
	}
	bi, errI := strconv.Atoi(v.Build)
	bj, errJ := strconv.Atoi(o.Build)
	if errI == nil && errJ == nil {
		return sign(bi - bj)
	}
	return strings.Compare(v.Build, o.Build)
}

// Follows reports whether the track moves from cur to next.
func (t Track) Follows(cur, next Version) bool {
	if next.CompareFull(cur) <= 0 {
		return false
	}
	switch t {
	case TrackExact:
		return false
	case TrackPatch, "":
		return next.Major == cur.Major && next.Minor == cur.Minor
	case TrackMinor:
		return next.Major == cur.Major
	case TrackLatest:
		return true
	}
	return false
}

// Valid reports whether t is a known track.
func (t Track) Valid() bool {
	switch t {
	case TrackExact, TrackPatch, TrackMinor, TrackLatest:
		return true
	}
	return false
}

// ExactRange matches one version.
func ExactRange(v Version) Range {
	return Range{groups: [][]comparator{{{op: "=", v: v}}}, text: v.String()}
}

// ParseRange reads comparators such as ">=1.3 <2", "^1.2", "~1.2.3",
// "1.x", or "*". A partial version fills the missing parts with 0.
func ParseRange(s string) (Range, error) {
	r := Range{text: strings.TrimSpace(s)}
	if r.text == "" {
		return r, fmt.Errorf("empty range")
	}
	for _, group := range strings.Split(r.text, "||") {
		var cs []comparator
		for _, tok := range strings.Fields(group) {
			parsed, err := parseComparator(tok)
			if err != nil {
				return Range{}, fmt.Errorf("range %q: %w", s, err)
			}
			cs = append(cs, parsed...)
		}
		if len(cs) == 0 {
			return Range{}, fmt.Errorf("range %q: empty group", s)
		}
		r.groups = append(r.groups, cs)
	}
	return r, nil
}

func (r Range) String() string { return r.text }

// Contains reports whether v is in the range.
func (r Range) Contains(v Version) bool {
	for _, g := range r.groups {
		ok := true
		for _, c := range g {
			if !c.matches(v) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func (c comparator) matches(v Version) bool {
	d := v.Compare(c.v)
	switch c.op {
	case ">":
		return d > 0
	case ">=":
		return d >= 0
	case "<":
		return d < 0
	case "<=":
		return d <= 0
	case "*":
		return true
	}
	return d == 0
}

func parseComparator(tok string) ([]comparator, error) {
	if tok == "*" || tok == "x" {
		return []comparator{{op: "*"}}, nil
	}
	op := ""
	for _, p := range []string{">=", "<=", ">", "<", "=", "^", "~"} {
		if strings.HasPrefix(tok, p) {
			op, tok = p, tok[len(p):]
			break
		}
	}
	v, parts, err := partialVersion(tok)
	if err != nil {
		return nil, err
	}
	switch {
	case op == "^":
		upper := Version{Major: v.Major + 1}
		if v.Major == 0 {
			upper = Version{Minor: v.Minor + 1}
		}
		return []comparator{{op: ">=", v: v}, {op: "<", v: upper}}, nil
	case op == "~" || (op == "" && parts == 2):
		return []comparator{{op: ">=", v: v}, {op: "<", v: Version{Major: v.Major, Minor: v.Minor + 1}}}, nil
	case op == "" && parts == 1:
		return []comparator{{op: ">=", v: v}, {op: "<", v: Version{Major: v.Major + 1}}}, nil
	case op == "":
		op = "="
	}
	return []comparator{{op: op, v: v}}, nil
}

// partialVersion reads "1", "1.2", "1.x", or a full version, and returns the
// count of the given parts.
func partialVersion(s string) (Version, int, error) {
	core, _, _ := strings.Cut(strings.TrimPrefix(s, "v"), "+")
	base, _, _ := strings.Cut(core, "-")
	parts := strings.Split(base, ".")
	for len(parts) > 0 && (parts[len(parts)-1] == "x" || parts[len(parts)-1] == "*") {
		parts = parts[:len(parts)-1]
	}
	if len(parts) == 0 || len(parts) > 3 {
		return Version{}, 0, fmt.Errorf("bad version %q", s)
	}
	if len(parts) == 3 {
		v, err := ParseVersion(s)
		return v, 3, err
	}
	nums := make([]int, 2)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return Version{}, 0, fmt.Errorf("bad version %q", s)
		}
		nums[i] = n
	}
	return Version{Major: nums[0], Minor: nums[1]}, len(parts), nil
}

func comparePre(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		ai, errA := strconv.Atoi(as[i])
		bi, errB := strconv.Atoi(bs[i])
		switch {
		case errA == nil && errB == nil:
			if ai != bi {
				return sign(ai - bi)
			}
		case errA == nil:
			return -1
		case errB == nil:
			return 1
		default:
			if c := strings.Compare(as[i], bs[i]); c != 0 {
				return c
			}
		}
	}
	return sign(len(as) - len(bs))
}

func sign(d int) int {
	switch {
	case d < 0:
		return -1
	case d > 0:
		return 1
	}
	return 0
}
