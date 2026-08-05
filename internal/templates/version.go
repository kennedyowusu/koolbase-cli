package templates

import (
	"fmt"
	"strconv"
	"strings"
)

// Environment is what the caller's machine and project actually have. A
// template declares the ranges it works with; this is what those ranges are
// checked against.
//
// Both fields may be empty when a version could not be determined — an
// unknown version SKIPS the check rather than failing it. Refusing to
// scaffold because `flutter --version` could not be parsed would be a worse
// outcome than installing a template that might need a newer Flutter.
type Environment struct {
	// FlutterVersion is the installed Flutter, e.g. "3.35.2".
	FlutterVersion string
	// SDKVersion is the koolbase_flutter version the template will be
	// generated against, e.g. "10.3.0".
	SDKVersion string
}

// Satisfies reports whether this environment meets an entry's declared
// constraints, returning a reason when it does not.
func (env Environment) Satisfies(e Entry) error {
	if err := checkConstraint("Flutter", env.FlutterVersion, e.FrameworkVersions); err != nil {
		return err
	}
	return checkConstraint("Koolbase SDK", env.SDKVersion, e.SDKVersions)
}

// checkConstraint evaluates a range like ">=3.35 <4.0" against a version.
//
// Deliberately a small subset: >=, >, <=, <, and bare equality, joined by
// spaces and ANDed. Templates are published by Koolbase, so the constraint
// vocabulary only has to cover what Koolbase writes — a full semver
// expression parser here would be a dependency and a bug surface bought for
// expressiveness nobody needs.
func checkConstraint(label, have, constraint string) error {
	constraint = strings.TrimSpace(constraint)
	if constraint == "" || have == "" {
		// No constraint declared, or no version known: nothing to enforce.
		return nil
	}

	for _, clause := range strings.Fields(constraint) {
		op, want := splitClause(clause)
		cmp := compareVersions(have, want)

		ok := false
		switch op {
		case ">=":
			ok = cmp >= 0
		case ">":
			ok = cmp > 0
		case "<=":
			ok = cmp <= 0
		case "<":
			ok = cmp < 0
		case "=", "":
			ok = cmp == 0
		default:
			// An operator this build does not understand must not silently
			// pass: a newer catalog could use a vocabulary we lack.
			return fmt.Errorf("unsupported version operator %q in constraint %q", op, constraint)
		}

		if !ok {
			return fmt.Errorf("needs %s %s, found %s", label, constraint, have)
		}
	}
	return nil
}

// splitClause separates ">=3.35" into (">=", "3.35").
func splitClause(clause string) (op, version string) {
	for _, prefix := range []string{">=", "<=", ">", "<", "="} {
		if strings.HasPrefix(clause, prefix) {
			return prefix, strings.TrimPrefix(clause, prefix)
		}
	}
	return "", clause
}

// compareVersions orders dotted numeric versions: -1, 0, or 1.
//
// Missing components count as zero, so "3.35" and "3.35.0" compare equal. A
// non-numeric component (a prerelease suffix like "3.35.0-beta.1") compares
// as its leading number, which is deliberate: a prerelease is close enough to
// its release for a range check, and the alternative is dragging in full
// semver precedence rules for a case Koolbase does not publish.
func compareVersions(a, b string) int {
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		av := numericAt(as, i)
		bv := numericAt(bs, i)
		if av != bv {
			if av < bv {
				return -1
			}
			return 1
		}
	}
	return 0
}

func numericAt(parts []string, i int) int {
	if i >= len(parts) {
		return 0
	}
	p := parts[i]
	// Take the leading digits: "0-beta" → 0, "12rc1" → 12.
	end := 0
	for end < len(p) && p[end] >= '0' && p[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0
	}
	v, err := strconv.Atoi(p[:end])
	if err != nil {
		return 0
	}
	return v
}
