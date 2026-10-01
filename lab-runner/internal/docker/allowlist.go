package docker

import "strings"

// Allowlist is the set of image reference prefixes a client may ask the runner
// to run (LAB_IMAGE_ALLOWLIST, comma-separated). Matching is on the full
// normalised reference including the tag, so "ghcr.io/bitforge/labs/" admits
// "ghcr.io/bitforge/labs/hello-flag:1" but not "ghcr.io/bitforge/labsx".
//
// An empty allowlist denies everything: the runner fails closed.
type Allowlist []string

// ParseAllowlist splits the env value; blank entries are dropped, entries are
// normalised the same way images are so "docker.io/library/" and "alpine"
// style prefixes compare correctly.
func ParseAllowlist(raw string) Allowlist {
	var out Allowlist
	for _, p := range strings.Split(raw, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, normalizeRef(p))
	}
	return out
}

// Allows reports whether image starts with one of the allowed prefixes.
func (a Allowlist) Allows(image string) bool {
	ref := normalizeRef(strings.TrimSpace(image))
	if ref == "" || strings.Contains(ref, "..") || strings.ContainsAny(ref, " \t\n") {
		return false
	}
	for _, prefix := range a {
		if strings.HasPrefix(ref, prefix) {
			return true
		}
	}
	return false
}

// normalizeRef expands Docker Hub shorthand so that "alpine:3" and
// "docker.io/library/alpine:3" are the same string. It deliberately does not
// touch tags or digests: the allowlist owner decides how specific to be.
func normalizeRef(ref string) string {
	if ref == "" {
		return ""
	}
	first := ref
	if i := strings.IndexByte(ref, '/'); i >= 0 {
		first = ref[:i]
	}
	// A registry host has a dot or a colon (port), or is "localhost".
	hasRegistry := strings.ContainsAny(first, ".:") || first == "localhost"
	if !hasRegistry {
		if !strings.Contains(ref, "/") {
			return "docker.io/library/" + ref
		}
		return "docker.io/" + ref
	}
	if strings.HasPrefix(ref, "index.docker.io/") {
		ref = "docker.io/" + strings.TrimPrefix(ref, "index.docker.io/")
	}
	return ref
}
