// Command check fails when a source file uses an error code that is not in spec/errors.json.
// Run it from the repository root: go run ./internal/errs/check [dir]. With no dir it scans the
// tracked source files (git ls-files); with a dir it walks that directory. It prints each unknown
// code with its file and line, and exits 1 when there are any.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Finding is one code that is not in the catalog, with where it is used.
type Finding struct {
	File string // path relative to the scanned root, slash-separated
	Line int    // 1-based
	Code string
}

var (
	codePattern = regexp.MustCompile(`HB-[A-Z0-9-]+`)
	sourceExts  = map[string]bool{".go": true, ".js": true, ".dart": true, ".kt": true, ".swift": true}
	generated   = map[string]bool{"codes_gen.go": true, "errors.gen.js": true, "errors.g.dart": true}
)

// Check returns one Finding for each code in the source files under root that is not in catalog.
// Generated files and node_modules are skipped.
func Check(root string, catalog map[string]bool) ([]Finding, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "node_modules" || d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return scan(root, files, catalog)
}

// scan checks the source files among files (slash-separated, relative to root).
func scan(root string, files []string, catalog map[string]bool) ([]Finding, error) {
	var out []Finding
	for _, rel := range files {
		if !sourceExts[filepath.Ext(rel)] || generated[filepath.Base(rel)] || strings.Contains("/"+rel, "/node_modules/") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return nil, err
		}
		for i, line := range strings.Split(string(data), "\n") {
			for _, code := range codePattern.FindAllString(line, -1) {
				if !catalog[code] {
					out = append(out, Finding{File: rel, Line: i + 1, Code: code})
				}
			}
		}
	}
	return out, nil
}

// tracked lists the files git tracks under the working directory, relative to it.
func tracked() ([]string, error) {
	out, err := exec.Command("git", "ls-files", "-z").Output()
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00"), nil
}

// loadCatalog returns the codes in spec/errors.json, found by walking up from the working directory.
func loadCatalog() (map[string]bool, error) {
	dir, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	for {
		data, err := os.ReadFile(filepath.Join(dir, "spec", "errors.json"))
		if err == nil {
			var doc struct {
				Codes []struct {
					Code string `json:"code"`
				} `json:"codes"`
			}
			if err := json.Unmarshal(data, &doc); err != nil {
				return nil, err
			}
			catalog := make(map[string]bool, len(doc.Codes))
			for _, c := range doc.Codes {
				catalog[c.Code] = true
			}
			return catalog, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, errors.New("spec/errors.json not found in the working directory or above")
		}
		dir = parent
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "check:", err)
	os.Exit(2)
}

func main() {
	catalog, err := loadCatalog()
	if err != nil {
		fail(err)
	}
	var findings []Finding
	if len(os.Args) > 1 {
		findings, err = Check(os.Args[1], catalog)
	} else {
		var files []string
		files, err = tracked()
		if err == nil {
			findings, err = scan(".", files, catalog)
		}
	}
	if err != nil {
		fail(err)
	}
	for _, f := range findings {
		fmt.Printf("%s:%d: %s is not in spec/errors.json\n", f.File, f.Line, f.Code)
	}
	if len(findings) > 0 {
		os.Exit(1)
	}
}
