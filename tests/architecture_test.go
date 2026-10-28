package tests

// Architecture checks: dependency rules between the engines, standalone
// modules without third-party dependencies,
// file names built only in core/layout, and no retired names in code.
// Paths are relative to the repository root, see repositoryRoot.

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const modulePrefix = "github.com/genesisdb-io/genesisdb/v2/packages/"

// repositoryRoot is the parent of this directory. Every path below is
// relative to it.
const repositoryRoot = ".."

func rootPath(elements ...string) string {
	return filepath.Join(append([]string{repositoryRoot}, elements...)...)
}

// allowedDependencies lists the GenesisDB modules each module may import.
var allowedDependencies = map[string][]string{
	"core":      {},
	"cache":     {},
	"storage":   {"core"},
	"index":     {"core"},
	"query":     {"core"},
	"schema":    {},
	"auth":      {},
	"metrics":   {},
	"telemetry": {},
	"store":     {"core", "storage", "index", "cache"},
	"server":    {"core", "store", "query", "schema", "auth"},
}

// engineModules must not import anything outside the standard library and
// GenesisDB, so embedding one engine pulls in no third-party code.
var engineModules = []string{"core", "cache", "storage", "index", "query", "store", "schema", "auth"}

func goFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(path, ".go") {
			files = append(files, path)
		}
		return nil
	})
	return files
}

func importsOf(t *testing.T, file string) []string {
	t.Helper()
	parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	var imports []string
	for _, spec := range parsed.Imports {
		path, _ := strconv.Unquote(spec.Path.Value)
		imports = append(imports, path)
	}
	return imports
}

func TestArchitecture_DependencyRules(t *testing.T) {
	for module, allowed := range allowedDependencies {
		isAllowed := map[string]bool{module: true}
		for _, dependency := range allowed {
			isAllowed[dependency] = true
		}
		for _, file := range goFiles(t, rootPath("packages", module)) {
			for _, imported := range importsOf(t, file) {
				if !strings.HasPrefix(imported, modulePrefix) {
					continue
				}
				dependency := strings.SplitN(strings.TrimPrefix(imported, modulePrefix), "/", 2)[0]
				if !isAllowed[dependency] {
					t.Errorf("%s imports %s, which module %s must not depend on", file, imported, module)
				}
			}
		}
	}
}

func TestArchitecture_EnginesHaveNoThirdPartyDependencies(t *testing.T) {
	for _, module := range engineModules {
		for _, file := range goFiles(t, rootPath("packages", module)) {
			for _, imported := range importsOf(t, file) {
				first := strings.SplitN(imported, "/", 2)[0]
				if strings.HasPrefix(imported, modulePrefix) || !strings.Contains(first, ".") {
					continue
				}
				t.Errorf("%s imports third-party package %s", file, imported)
			}
		}
	}
}

func TestArchitecture_FileExtensionsOnlyInLayout(t *testing.T) {
	pattern := regexp.MustCompile(`"[^"]*\.(eventstore|gdb)"`)
	for _, file := range goFiles(t, repositoryRoot) {
		if strings.HasSuffix(file, "_test.go") || strings.Contains(file, filepath.Join("core", "layout")) {
			continue
		}
		content, _ := os.ReadFile(file)
		if match := pattern.Find(content); match != nil {
			t.Errorf("%s hard-codes a file name: %s", file, match)
		}
	}
}

// Retired terms must not appear in code or docs, except in the files that
// document the old names as legacy.
func TestArchitecture_NoRetiredTerms(t *testing.T) {
	retired := map[string]*regexp.Regexp{
		"GQL":       regexp.MustCompile(`\bG[Qq][Ll]\b|GQL[A-Z]|Gql`),
		"gRPC":      regexp.MustCompile(`(?i)\bgrpc\b`),
		"license":   regexp.MustCompile(`(?i)license (key|check|status)|GENESISDB_LICENSE[^"]`),
		"edition":   regexp.MustCompile(`(?i)enterprise edition|community edition`),
		"singleton": regexp.MustCompile(`GetEventStore|InitEventStore`),
	}
	allowedFiles := map[string]bool{
		rootPath("tests", "architecture_test.go"): true,
		rootPath("cmd", "genesisdb", "config.go"): true,
		rootPath("CHANGELOG.md"):                  true,
		rootPath("ROADMAP.md"):                    true,
		rootPath("docs", "compatibility.md"):      true,
	}
	var files []string
	for _, pattern := range []string{"*.go", "*.md"} {
		matches, _ := filepath.Glob(rootPath(pattern))
		files = append(files, matches...)
	}
	files = append(files, goFiles(t, rootPath("packages"))...)
	files = append(files, goFiles(t, rootPath("cmd"))...)
	files = append(files, goFiles(t, rootPath("tests"))...)
	docs, _ := filepath.Glob(rootPath("docs", "*.md"))
	files = append(files, docs...)
	for _, file := range files {
		if allowedFiles[file] || strings.Contains(file, "testdata") {
			continue
		}
		content, _ := os.ReadFile(file)
		for name, pattern := range retired {
			if location := pattern.FindIndex(content); location != nil {
				line := strings.Count(string(content[:location[0]]), "\n") + 1
				t.Errorf("%s:%d uses the retired term %s: %q", file, line, name, content[location[0]:location[1]])
			}
		}
	}
}
