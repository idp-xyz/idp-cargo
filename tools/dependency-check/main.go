// dependency-check enforces the package rules of the repository structure baseline: the executable
// context map and layer rules D1–D6, the api leaf rule (AC2, AC3), and CODEOWNERS coverage of every
// bounded context and the kernel (AC7). D7 (the <bc>api import alias) is a "should" rule and only
// produces warnings.
//
// The cross-context allowlist is generated from the Context Map's relationships with -generate and
// lists only contexts that exist under internal/contexts.
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

func main() {
	allowPath := flag.String("allowlist", "tools/dependency-check/allowlist.txt", "generated cross-context import allowlist")
	ownersPath := flag.String("codeowners", ".github/CODEOWNERS", "CODEOWNERS file that must cover every context and the kernel")
	contextMap := flag.String("generate", "", "write the allowlist from this Context Map JSON instead of checking")
	flag.Parse()
	var err error
	if *contextMap != "" {
		err = runGenerate(*contextMap, *allowPath)
	} else {
		err = runCheck(*allowPath, *ownersPath)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "dependency-check:", err)
		os.Exit(1)
	}
}

// listedPackage is the part of `go list -json` output the rules need.
type listedPackage struct {
	ImportPath   string
	Dir          string
	GoFiles      []string
	TestGoFiles  []string
	XTestGoFiles []string
	Imports      []string
	TestImports  []string
	XTestImports []string
}

type finding struct {
	rule, pkg, detail string
}

func (f finding) String() string { return fmt.Sprintf("%s %s: %s", f.rule, f.pkg, f.detail) }

func runCheck(allowPath, ownersPath string) error {
	module, err := goOutput("list", "-m")
	if err != nil {
		return err
	}
	module = strings.TrimSpace(module)
	raw, err := goOutput("list", "-tags", "integration", "-json", "./...")
	if err != nil {
		return err
	}
	pkgs, err := decodePackages(strings.NewReader(raw))
	if err != nil {
		return err
	}
	allowText, err := os.ReadFile(allowPath)
	if err != nil {
		return err
	}
	allow, err := parseAllowlist(string(allowText))
	if err != nil {
		return err
	}
	violations := checkImports(module, pkgs, allow)
	required, err := requiredOwnedDirs(".")
	if err != nil {
		return err
	}
	owners, err := os.ReadFile(ownersPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	violations = append(violations, checkCodeowners(string(owners), required)...)
	for _, w := range checkAliases(module, pkgs) {
		fmt.Println("warning", w)
	}
	if len(violations) > 0 {
		for _, v := range violations {
			fmt.Println(v)
		}
		return fmt.Errorf("%d violation(s)", len(violations))
	}
	fmt.Printf("dependency-check: %d packages, %d allowlisted context relationships, CODEOWNERS covers %s; no violations\n",
		len(pkgs), len(allow), strings.Join(required, ", "))
	return nil
}

func goOutput(args ...string) (string, error) {
	cmd := exec.Command("go", args...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("go %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}

func decodePackages(r io.Reader) ([]listedPackage, error) {
	var pkgs []listedPackage
	dec := json.NewDecoder(r)
	for {
		var p listedPackage
		err := dec.Decode(&p)
		if err == io.EOF {
			return pkgs, nil
		}
		if err != nil {
			return nil, err
		}
		pkgs = append(pkgs, p)
	}
}

// Layers of the baseline's runtime layering (§05).
const (
	layerEntry      = "entry"
	layerBootstrap  = "bootstrap"
	layerContext    = "context"
	layerKernel     = "kernel"
	layerReference  = "reference"
	layerFoundation = "foundation"
	layerInbound    = "inbound"
	layerTool       = "tool"
	layerUnknown    = "unknown"
)

// Parts of a bounded context (§07).
const (
	partModule = "module"
	partAPI    = "api"
	partDomain = "domain"
	partOther  = "internal"
	partStray  = "stray"
)

type class struct {
	layer, context, part string
}

func classify(module, importPath string) class {
	rel := strings.TrimPrefix(importPath, module+"/")
	seg := strings.Split(rel, "/")
	switch {
	case seg[0] == "cmd":
		return class{layer: layerEntry}
	case seg[0] == "tools":
		return class{layer: layerTool}
	case seg[0] != "internal" || len(seg) < 2:
		return class{layer: layerUnknown}
	}
	switch seg[1] {
	case "bootstrap":
		return class{layer: layerBootstrap}
	case "kernel":
		return class{layer: layerKernel}
	case "foundation":
		return class{layer: layerFoundation}
	case "transport", "ai", "processes":
		return class{layer: layerInbound}
	case "reference":
		if len(seg) > 2 && seg[2] == "api" {
			return class{layer: layerReference, part: partAPI}
		}
		return class{layer: layerReference}
	case "contexts":
		if len(seg) < 3 {
			return class{layer: layerUnknown}
		}
		c := class{layer: layerContext, context: seg[2]}
		switch {
		case len(seg) == 3:
			c.part = partModule
		case seg[3] == "api":
			c.part = partAPI
		case seg[3] == "internal" && len(seg) > 4 && seg[4] == "domain":
			c.part = partDomain
		case seg[3] == "internal":
			c.part = partOther
		default:
			c.part = partStray
		}
		return c
	}
	return class{layer: layerUnknown}
}

func isStandard(importPath string) bool {
	return !strings.Contains(strings.SplitN(importPath, "/", 2)[0], ".")
}

func checkImports(module string, pkgs []listedPackage, allow map[[2]string]bool) []finding {
	var out []finding
	for _, p := range pkgs {
		from := classify(module, p.ImportPath)
		name := strings.TrimPrefix(p.ImportPath, module+"/")
		switch {
		case from.layer == layerUnknown:
			out = append(out, finding{"layer", name, "package is in no layer of the baseline (§05)"})
			continue
		case from.layer == layerContext && from.part == partStray:
			out = append(out, finding{"layout", name, "a context holds only module.go, api/ and internal/ (§07)"})
			continue
		}
		seen := map[string]bool{}
		for _, imp := range append(append(append([]string{}, p.Imports...), p.TestImports...), p.XTestImports...) {
			if seen[imp] || imp == p.ImportPath {
				continue
			}
			seen[imp] = true
			if f, bad := judge(module, from, imp, allow); bad {
				f.pkg = name
				out = append(out, f)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// judge decides one import. It returns the finding and true for a violation.
func judge(module string, from class, imp string, allow map[[2]string]bool) (finding, bool) {
	if isStandard(imp) {
		return finding{}, false
	}
	ours := imp == module || strings.HasPrefix(imp, module+"/")
	target := strings.TrimPrefix(imp, module+"/")
	bad := func(rule, detail string) (finding, bool) { return finding{rule: rule, detail: detail}, true }
	if !ours {
		switch {
		case from.layer == layerKernel:
			return bad("kernel", "imports "+imp+"; the kernel depends on the standard library only")
		case from.layer == layerContext && from.part == partAPI:
			return bad("D2", "imports "+imp+"; api packages import only the standard library and the kernel (AC3)")
		case from.layer == layerContext && from.part == partDomain:
			return bad("domain", "imports "+imp+"; the domain depends on the standard library and the kernel only (§07)")
		}
		return finding{}, false
	}
	to := classify(module, imp)
	switch from.layer {
	case layerBootstrap, layerTool:
		return finding{}, false
	case layerEntry:
		if to.layer != layerBootstrap {
			return bad("entry", "imports "+target+"; process entry points reach Cargo code through internal/bootstrap (§05)")
		}
	case layerKernel:
		if to.layer != layerKernel {
			return bad("kernel", "imports "+target+"; the kernel depends on the standard library only")
		}
	case layerFoundation:
		if to.layer != layerKernel {
			return bad("D5", "imports "+target+"; foundation imports only external modules, the standard library and the kernel")
		}
	case layerReference:
		if to.layer != layerKernel && to.layer != layerFoundation && to.layer != layerReference {
			return bad("reference", "imports "+target+"; reference data depends on the kernel and foundation only")
		}
	case layerInbound:
		ok := to.layer == layerKernel || to.layer == layerFoundation || to.layer == layerInbound ||
			(to.layer == layerContext && to.part == partAPI)
		if !ok {
			return bad("D6", "imports "+target+"; transport, ai and processes import only context api packages, kernel and foundation")
		}
	case layerContext:
		return judgeContext(from, to, target, allow)
	}
	return finding{}, false
}

func judgeContext(from, to class, target string, allow map[[2]string]bool) (finding, bool) {
	bad := func(rule, detail string) (finding, bool) { return finding{rule: rule, detail: detail}, true }
	switch from.part {
	case partAPI:
		if to.layer != layerKernel {
			return bad("D2", "imports "+target+"; api packages import only the standard library and the kernel (AC3)")
		}
		return finding{}, false
	case partDomain:
		if to.layer != layerKernel && !(to.layer == layerContext && to.context == from.context && to.part == partDomain) {
			return bad("domain", "imports "+target+"; the domain depends on the standard library and the kernel only (§07)")
		}
		return finding{}, false
	}
	switch to.layer {
	case layerKernel, layerFoundation:
		return finding{}, false
	case layerReference:
		if to.part == partAPI {
			return finding{}, false
		}
		return bad("D1", "imports "+target+"; contexts use reference data through internal/reference/api")
	case layerBootstrap, layerInbound:
		return bad("D4", "imports "+target+"; contexts do not import transport, ai, processes or bootstrap")
	case layerContext:
		if to.context == from.context {
			return finding{}, false
		}
		if to.part != partAPI {
			return bad("D1", "imports "+target+"; other contexts are reachable only through their api package")
		}
		if !allow[[2]string{from.context, to.context}] {
			return bad("D1", fmt.Sprintf("imports %s; %s is not an upstream of %s in the allowlist generated from the Context Map (AC2)", target, to.context, from.context))
		}
		return finding{}, false
	}
	return bad("layer", "imports "+target+", which no context may import")
}

// parseAllowlist reads "downstream upstream" lines.
func parseAllowlist(text string) (map[[2]string]bool, error) {
	allow := map[[2]string]bool{}
	for n, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 2 {
			return nil, fmt.Errorf("allowlist line %d: want \"downstream upstream\", got %q", n+1, line)
		}
		allow[[2]string{f[0], f[1]}] = true
	}
	return allow, nil
}

// requiredOwnedDirs lists the directories CODEOWNERS must name: every context and the kernel.
func requiredOwnedDirs(root string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(root, "internal", "contexts"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, "internal/contexts/"+e.Name()+"/")
		}
	}
	if info, err := os.Stat(filepath.Join(root, "internal", "kernel")); err == nil && info.IsDir() {
		dirs = append(dirs, "internal/kernel/")
	}
	sort.Strings(dirs)
	return dirs, nil
}

// checkCodeowners requires an explicit CODEOWNERS entry with at least one owner for each directory.
func checkCodeowners(text string, required []string) []finding {
	owned := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 0 || strings.HasPrefix(f[0], "#") {
			continue
		}
		dir := strings.TrimSuffix(strings.TrimPrefix(f[0], "/"), "**")
		if !strings.HasSuffix(dir, "/") {
			dir += "/"
		}
		owned[dir] = len(f) > 1
	}
	var out []finding
	for _, dir := range required {
		if !owned[dir] {
			out = append(out, finding{"AC7", dir, "CODEOWNERS needs an explicit entry with an owner for this directory"})
		}
	}
	return out
}

// checkAliases warns when code outside a context imports that context's api without the <bc>api alias (D7).
func checkAliases(module string, pkgs []listedPackage) []finding {
	var out []finding
	fset := token.NewFileSet()
	for _, p := range pkgs {
		from := classify(module, p.ImportPath)
		files := append(append(append([]string{}, p.GoFiles...), p.TestGoFiles...), p.XTestGoFiles...)
		for _, name := range files {
			where := strings.TrimPrefix(p.ImportPath, module+"/") + "/" + name
			file, err := parser.ParseFile(fset, filepath.Join(p.Dir, name), nil, parser.ImportsOnly)
			if err != nil {
				out = append(out, finding{"D7", where, err.Error()})
				continue
			}
			for _, spec := range file.Imports {
				imp, _ := strconv.Unquote(spec.Path.Value)
				to := classify(module, imp)
				if to.layer != layerContext || to.part != partAPI || (from.layer == layerContext && from.context == to.context) {
					continue
				}
				if want := to.context + "api"; spec.Name == nil || spec.Name.Name != want {
					out = append(out, finding{"D7", where, fmt.Sprintf("import %s as %s", imp, want)})
				}
			}
		}
	}
	return out
}

// contextMapDoc is the part of the Context Map JSON the generator reads.
type contextMapDoc struct {
	Version       string `json:"version"`
	Relationships []struct {
		From string `json:"frm"`
		To   string `json:"to"`
	} `json:"relationships"`
}

// generate returns the allowlist lines for the contexts present: the downstream context may import the
// upstream context's api.
func generate(doc contextMapDoc, present []string) []string {
	have := map[string]bool{}
	for _, c := range present {
		have[c] = true
	}
	lines := map[string]bool{}
	for _, r := range doc.Relationships {
		up, down := strings.ToLower(r.From), strings.ToLower(r.To)
		if have[up] && have[down] && up != down {
			lines[down+" "+up] = true
		}
	}
	var out []string
	for l := range lines {
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

func runGenerate(contextMapPath, allowPath string) error {
	raw, err := os.ReadFile(contextMapPath)
	if err != nil {
		return err
	}
	var doc contextMapDoc
	if err = json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	if len(doc.Relationships) == 0 {
		return fmt.Errorf("%s has no relationships", contextMapPath)
	}
	dirs, err := requiredOwnedDirs(".")
	if err != nil {
		return err
	}
	var present []string
	for _, d := range dirs {
		if c, ok := strings.CutPrefix(d, "internal/contexts/"); ok {
			present = append(present, strings.TrimSuffix(c, "/"))
		}
	}
	sum := sha256.Sum256(raw)
	var b strings.Builder
	fmt.Fprintf(&b, "# Generated by `go run ./tools/dependency-check -generate <Context Map JSON>`; do not edit.\n")
	fmt.Fprintf(&b, "# Context Map version %s, sha256 %s, %d relationships; contexts present: %s.\n",
		doc.Version, hex.EncodeToString(sum[:]), len(doc.Relationships), strings.Join(present, ", "))
	fmt.Fprintf(&b, "# Each line lets the downstream context import the upstream context's api package.\n")
	fmt.Fprintf(&b, "# downstream upstream\n")
	for _, l := range generate(doc, present) {
		fmt.Fprintln(&b, l)
	}
	return os.WriteFile(allowPath, []byte(b.String()), 0o644)
}
