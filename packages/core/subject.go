package core

import "strings"

// SubjectAndAncestors returns every hierarchy key a subject is indexed under:
// the root "/" (for subjects starting with "/"), every prefix that ends right
// before a "/" (except at position 0), and the subject itself. Values are
// never normalized.
//
// An exact match on one of these keys is equivalent to
// subject == key || strings.HasPrefix(subject, key+"/"), where the root "/"
// stands for every subject starting with "/".
func SubjectAndAncestors(subject string) []string {
	keys := make([]string, 0, 4)
	seen := make(map[string]struct{}, 4)
	add := func(key string) {
		if _, duplicate := seen[key]; duplicate {
			return
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	if strings.HasPrefix(subject, "/") {
		add("/")
	}
	for position := 1; position < len(subject); position++ {
		if subject[position] == '/' {
			add(subject[:position])
		}
	}
	add(subject)
	return keys
}

// IsDirectChild reports whether subject is a direct child of parent
// (GDBQL operator UNDER). The parent itself is not a child of itself.
func IsDirectChild(subject, parent string) bool {
	if parent == "/" {
		return len(subject) > 1 && subject[0] == '/' && !strings.Contains(subject[1:], "/")
	}
	if !strings.HasPrefix(subject, parent+"/") {
		return false
	}
	return !strings.Contains(subject[len(parent)+1:], "/")
}

// IsStrictDescendant reports whether subject is a descendant of parent at any
// depth (GDBQL operator DESCENDANTS). The parent itself is excluded.
func IsStrictDescendant(subject, parent string) bool {
	if parent == "/" {
		return len(subject) > 1 && subject[0] == '/'
	}
	return strings.HasPrefix(subject, parent+"/")
}

// TopLevelSubject returns the first path segment of a subject with a leading
// slash, for example "/user" for "/user/123".
func TopLevelSubject(subject string) string {
	segments := strings.Split(strings.Trim(subject, "/"), "/")
	return "/" + segments[0]
}
