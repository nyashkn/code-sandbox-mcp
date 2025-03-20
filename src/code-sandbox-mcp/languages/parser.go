package languages

import (
	"regexp"
	"strings"
)

var (
	// Python import patterns
	pythonImportRe  = regexp.MustCompile(`(?m)^import\s+(\w+)`)
	pythonFromRe    = regexp.MustCompile(`(?m)^from\s+(\w+)\s+import`)
	pythonDynamicRe = regexp.MustCompile(`__import__\(['"](\w+)['"]\)`)
	// Requirements comment pattern
	pythonRequirementsRe = regexp.MustCompile(`(?m)^#\s*requirements:\s*(.+)$`)

	// Node.js import patterns
	nodeRequireRe = regexp.MustCompile(`(?m)require\(['"]([^'"]+)['"]\)`)
	nodeImportRe  = regexp.MustCompile(`(?m)import\s+(?:\{[^}]*\}|\*\s+as\s+\w+|\w+)\s+from\s+['"]([^'"]+)['"]`)
	nodeDynamicRe = regexp.MustCompile(`(?m)import\(['"]([^'"]+)['"]\)`)

	// Go import patterns
	goSingleImportRe = regexp.MustCompile(`(?m)^import\s+"([^"]+)"`)
	goGroupImportRe  = regexp.MustCompile(`(?m)^[^/]*"([^"]+)"`)

	// Standard library packages
	pythonStdLib = map[string]bool{
		"os": true, "sys": true, "datetime": true, "json": true, "math": true,
		"random": true, "re": true, "time": true, "collections": true, "pathlib": true,
		// Add more as needed
	}

	nodeStdLib = map[string]bool{
		"fs": true, "path": true, "http": true, "https": true, "crypto": true,
		"buffer": true, "stream": true, "util": true, "events": true, "os": true,
		// Add more as needed
	}

	goStdLib = map[string]bool{
		"fmt": true, "os": true, "io": true, "strings": true, "time": true,
		"net/http": true, "encoding/json": true, "path/filepath": true,
		// Add more as needed
	}

	// Package name mappings (for cases where import name differs from package name)
	pythonPkgMap = map[string]string{
		"PIL": "pillow",
	}
)

// parseRequirements parses comma-separated package requirements
func parseRequirements(requirementsStr string) []string {
	// Split by comma and trim whitespace
	rawReqs := strings.Split(requirementsStr, ",")
	reqs := make([]string, 0, len(rawReqs))

	for _, req := range rawReqs {
		req = strings.TrimSpace(req)
		if req != "" {
			reqs = append(reqs, req)
		}
	}

	return reqs
}

// ParsePythonImports extracts non-standard library package imports from Python code
func ParsePythonImports(code string) []string {
	imports := make(map[string]bool)

	// Find standard imports
	for _, match := range pythonImportRe.FindAllStringSubmatch(code, -1) {
		pkg := match[1]
		if mapped, ok := pythonPkgMap[pkg]; ok {
			pkg = mapped
		}
		if !pythonStdLib[pkg] {
			imports[pkg] = true
		}
	}

	// Find from imports
	for _, match := range pythonFromRe.FindAllStringSubmatch(code, -1) {
		pkg := match[1]
		if mapped, ok := pythonPkgMap[pkg]; ok {
			pkg = mapped
		}
		if !pythonStdLib[pkg] {
			imports[pkg] = true
		}
	}

	// Find dynamic imports
	for _, match := range pythonDynamicRe.FindAllStringSubmatch(code, -1) {
		pkg := match[1]
		if mapped, ok := pythonPkgMap[pkg]; ok {
			pkg = mapped
		}
		if !pythonStdLib[pkg] {
			imports[pkg] = true
		}
	}

	// Find requirements comments
	for _, match := range pythonRequirementsRe.FindAllStringSubmatch(code, -1) {
		requirementsStr := match[1]
		reqs := parseRequirements(requirementsStr)
		for _, req := range reqs {
			// For requirements we don't filter standard library
			// This allows users to specify specific versions of standard lib packages
			imports[req] = true
		}
	}

	return mapToSlice(imports)
}

// ParseNodeImports extracts non-standard library package imports from Node.js code
func ParseNodeImports(code string) []string {
	imports := make(map[string]bool)

	// Find require statements
	for _, match := range nodeRequireRe.FindAllStringSubmatch(code, -1) {
		pkg := getBasePackage(match[1])
		if !nodeStdLib[pkg] {
			imports[pkg] = true
		}
	}

	// Find ES6 imports
	for _, match := range nodeImportRe.FindAllStringSubmatch(code, -1) {
		pkg := getBasePackage(match[1])
		if !nodeStdLib[pkg] {
			imports[pkg] = true
		}
	}

	// Find dynamic imports
	for _, match := range nodeDynamicRe.FindAllStringSubmatch(code, -1) {
		pkg := getBasePackage(match[1])
		if !nodeStdLib[pkg] {
			imports[pkg] = true
		}
	}

	return mapToSlice(imports)
}

// ParseGoImports extracts non-standard library package imports from Go code
func ParseGoImports(code string) []string {
	imports := make(map[string]bool)

	// Find single-line imports
	for _, match := range goSingleImportRe.FindAllStringSubmatch(code, -1) {
		pkg := match[1]
		if !goStdLib[pkg] {
			imports[pkg] = true
		}
	}

	// Find imports in import groups
	for _, match := range goGroupImportRe.FindAllStringSubmatch(code, -1) {
		pkg := match[1]
		if !goStdLib[pkg] {
			imports[pkg] = true
		}
	}

	return mapToSlice(imports)
}

// Helper function to convert a map[string]bool to []string
func mapToSlice(m map[string]bool) []string {
	result := make([]string, 0, len(m))
	for k := range m {
		result = append(result, k)
	}
	return result
}

// Helper function to get the base package name from a Node.js import path
func getBasePackage(path string) string {
	// Handle scoped packages (@org/pkg)
	if strings.HasPrefix(path, "@") {
		parts := strings.Split(path, "/")
		if len(parts) >= 2 {
			return strings.Join(parts[:2], "/")
		}
	}
	// Handle regular packages (possibly with submodules)
	return strings.Split(path, "/")[0]
}
