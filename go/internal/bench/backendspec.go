package bench

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// BackendTypes are the implementations a backend spec may name.
var BackendTypes = []string{"module", "lua", "native"}

// BackendSpec is one -backends entry: [name=]type[@host:port].
type BackendSpec struct {
	// Name identifies the backend in results; it defaults to Type.
	Name string
	Type string
	// Addr is empty when the spec gives none.
	Addr string
}

// backendName keeps names path-safe: they prefix artifact directory names.
var backendName = regexp.MustCompile(`^[a-z0-9][a-z0-9._+-]*$`)

// ParseBackendSpecs parses a comma-separated -backends value. Names must be
// unique, so several builds of one type are told apart by name, for example
// "develop=module@127.0.0.1:26380,lazy=module@127.0.0.1:26381,lua@127.0.0.1:26379".
func ParseBackendSpecs(list string) ([]BackendSpec, error) {
	var specs []BackendSpec
	seen := map[string]bool{}
	for _, raw := range strings.Split(list, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		spec, err := parseBackendSpec(raw)
		if err != nil {
			return nil, err
		}
		if seen[spec.Name] {
			return nil, fmt.Errorf("backend %q: name given twice; name each build, as in name=%s@host:port", spec.Name, spec.Type)
		}
		seen[spec.Name] = true
		specs = append(specs, spec)
	}
	if len(specs) == 0 {
		return nil, fmt.Errorf("no backends in %q", list)
	}
	return specs, nil
}

func parseBackendSpec(raw string) (BackendSpec, error) {
	rest, addr, hasAddr := strings.Cut(raw, "@")
	if hasAddr && addr == "" {
		return BackendSpec{}, fmt.Errorf("backend %q: empty address after @", raw)
	}
	name, typ, named := strings.Cut(rest, "=")
	if !named {
		name, typ = rest, rest
	}
	if !slices.Contains(BackendTypes, typ) {
		return BackendSpec{}, fmt.Errorf("backend %q: unknown type %q (want one of %s)", raw, typ, strings.Join(BackendTypes, ", "))
	}
	if !backendName.MatchString(name) {
		return BackendSpec{}, fmt.Errorf("backend %q: name %q must match %s", raw, name, backendName)
	}
	return BackendSpec{Name: name, Type: typ, Addr: addr}, nil
}
