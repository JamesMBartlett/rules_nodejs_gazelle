// Copyright 2019 The Bazel Authors. All rights reserved.
// Modifications copyright (C) 2021 BenchSci Analytics Inc.
// Modifications copyright (C) 2018 Ecosia GmbH

// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at

// http://www.apache.org/licenses/LICENSE-2.0

// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package js

import (
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/label"
	"github.com/bazelbuild/bazel-gazelle/repo"
	"github.com/bazelbuild/bazel-gazelle/resolve"
	"github.com/bazelbuild/bazel-gazelle/rule"
)

// BUILTINS list taken from https://github.com/sindresorhus/builtin-modules/blob/master/builtin-modules.json
var BUILTINS = map[string]bool{
	"assert":         true,
	"async_hooks":    true,
	"buffer":         true,
	"child_process":  true,
	"cluster":        true,
	"console":        true,
	"constants":      true,
	"crypto":         true,
	"dgram":          true,
	"dns":            true,
	"domain":         true,
	"events":         true,
	"fs":             true,
	"http":           true,
	"http2":          true,
	"https":          true,
	"inspector":      true,
	"module":         true,
	"net":            true,
	"os":             true,
	"path":           true,
	"perf_hooks":     true,
	"process":        true,
	"punycode":       true,
	"querystring":    true,
	"readline":       true,
	"repl":           true,
	"stream":         true,
	"string_decoder": true,
	"timers":         true,
	"tls":            true,
	"trace_events":   true,
	"tty":            true,
	"url":            true,
	"util":           true,
	"v8":             true,
	"vm":             true,
	"wasi":           true,
	"worker_threads": true,
	"zlib":           true,
}

// maps resolve.Resolver -> *JS
// Resolver is an interface that language extensions can implement to resolve
// dependencies in rules they generate.

// Imports returns a list of ImportSpecs that can be used to import the rule
// r. This is used to populate RuleIndex.
//
// If nil is returned, the rule will not be indexed. If any non-nil slice is
// returned, including an empty slice, the rule will be indexed.
func (lang *JS) Imports(c *config.Config, r *rule.Rule, f *rule.File) []resolve.ImportSpec {

	jsConfigs := c.Exts[languageName].(JsConfigs)
	jsConfig := jsConfigs[f.Pkg]

	srcs := r.AttrStrings("srcs")

	importSpecs := make([]resolve.ImportSpec, 0)

	// index each source file
	for _, src := range srcs {
		filePath := path.Join(f.Pkg, src)
		importSpecs = append(importSpecs, resolve.ImportSpec{
			Lang: lang.Name(),
			Imp:  filePath,
		})
	}

	isBarrel := false
	// look for index.js and mark this rule as a module rule
	for _, src := range srcs {
		if isBarrelFile(src) {
			isBarrel = true
			break
		}
	}

	// modules can be resolved via the directory containing them
	if isBarrel {
		importSpecs = append(importSpecs, resolve.ImportSpec{
			Lang: lang.Name(),
			Imp:  f.Pkg,
		})
	}

	// Any subfolders could be used to depend on this rule
	folderImports := jsConfig.CollectAll && (r.Kind() == getKind(c, "ts_project") || r.Kind() == getKind(c, "js_library"))
	if folderImports {
		base := filepath.Dir(f.Path)
		subDirectories := make(map[string]bool)
		for _, src := range srcs {
			dir := filepath.Dir(src)
			subDirectories[dir] = true
		}
		for subDirectory, _ := range subDirectories {
			root := strings.TrimSuffix(base, f.Pkg)
			relPath := strings.TrimPrefix(subDirectory, root)
			path := fmt.Sprintf("%s/%s", f.Pkg, relPath)
			importSpecs = append(importSpecs, resolve.ImportSpec{
				Lang: lang.Name(),
				Imp:  path,
			})
		}
	}

	return importSpecs
}

// Embeds returns a list of labels of rules that the given rule embeds. If
// a rule is embedded by another importable rule of the same language, only
// the embedding rule will be indexed. The embedding rule will inherit
// the imports of the embedded rule.
func (*JS) Embeds(r *rule.Rule, from label.Label) []label.Label {
	return nil
}

// Resolve translates imported libraries for a given rule into Bazel
// dependencies. Information about imported libraries is returned for each
// rule generated by language.GenerateRules in
// language.GenerateResult.Imports. Resolve generates a "deps" attribute (or
// the appropriate language-specific equivalent) for each import according to
// language-specific rules and heuristics.
// https://www.typescriptlang.org/docs/handbook/module-resolution.html#classic
func (lang *JS) Resolve(c *config.Config, ix *resolve.RuleIndex, rc *repo.RemoteCache, r *rule.Rule, _imports interface{}, from label.Label) {

	jsConfigs := c.Exts[languageName].(JsConfigs)
	jsConfig := jsConfigs[from.Pkg]

	packageJSON := "//:package"
	packageResolveResult := lang.tryResolve("package.json", c, ix, from)
	if packageResolveResult.err != nil {
		log.Print(Err("%v", packageResolveResult.err))
		return
	}
	if packageResolveResult.selfImport {
		// ignore self imports
		return
	}
	if packageResolveResult.label != label.NoLabel {
		// add discovered label
		lbl := packageResolveResult.label
		packageJSON = lbl.Abs(from.Repo, from.Pkg).String()
	}

	imports := _imports.(*imports)
	depSet := make(map[string]bool)
	dataSet := make(map[string]bool)
	for name := range imports.set {

		// is it a package.json import?
		if name == "package" || name == "package.json" {
			depSet[packageJSON] = true
			continue
		}

		// fix aliases
		match := jsConfig.ImportAliasPattern.FindStringSubmatch(name)
		if len(match) > 0 {
			prefix := match[0]
			alias := ""
			for _, impAlias := range jsConfig.ImportAliases {
				if impAlias.From == prefix {
					alias = impAlias.To
					break
				}
			}

			name = alias + strings.TrimPrefix(name, prefix)
		}

		// is it an npm dependency?
		isNpm, npmLabel, devDep := lang.isNpmDependency(name, jsConfig)
		if isNpm {

			s := strings.Split(name, "/")
			name = s[0]
			if strings.HasPrefix(name, "@") && len(s) >= 2 {
				name += "/" + s[1]
			}
			depSet[fmt.Sprintf("%s%s", npmLabel, name)] = true
			if !devDep {
				// Runtime dependency
				dataSet[fmt.Sprintf("%s%s", npmLabel, name)] = true
			}

			if jsConfig.LookupTypes && r.Kind() == "ts_project" {
				// does it have a corresponding @types/[...] declaration?
				typesFound, npmLabel, _ := lang.isNpmDependency("@types/"+name, jsConfig)
				if typesFound {
					depSet[fmt.Sprintf("%s@types/%s", npmLabel, name)] = true
				}
			}

			continue
		}

		// is it a builtin?
		if strings.HasPrefix(name, "node:") {
			if jsConfig.LookupTypes && r.Kind() == "ts_project" {
				typesFound, npmLabel, _ := lang.isNpmDependency("@types/node", jsConfig)
				if typesFound {
					depSet[fmt.Sprintf("%s@types/node", npmLabel)] = true
				}
			}
			continue
		}
		if _, ok := BUILTINS[name]; ok {
			// add @types/node when using node.js builtin and have @types/nodes installed
			if jsConfig.LookupTypes && r.Kind() == "ts_project" {
				// does it have a corresponding @types/[...] declaration?
				typesFound, npmLabel, _ := lang.isNpmDependency("@types/"+name, jsConfig)
				if typesFound {
					depSet[fmt.Sprintf("%s@types/%s", npmLabel, name)] = true
				}
			}
			continue
		}

		// Is user resolved
		resolveResult := lang.tryResolve(name, c, ix, from)
		if resolveResult.err == nil && !resolveResult.selfImport && resolveResult.label != label.NoLabel {
			// add discovered label
			lbl := resolveResult.label
			dep := lbl.Rel(from.Repo, from.Pkg).String()
			depSet[dep] = true
			continue
		}

		lang.resolveWalkParents(name, depSet, dataSet, c, ix, rc, r, from)
	}

	// Add in additional jest dependencies
	if r.Kind() == getKind(c, "jest_test") {
		for name, npmLabel := range jsConfig.NpmDependencies.DevDependencies {
			if name == "jest-cli" || name == "jest-junit" {
				continue
			}
			if strings.HasPrefix(name, "@types/jest") {
				depSet[fmt.Sprintf("%s%s", npmLabel, name)] = true
			}
			if strings.HasPrefix(name, "jest") {
				depSet[fmt.Sprintf("%s%s", npmLabel, name)] = true
				dataSet[fmt.Sprintf("%s%s", npmLabel, name)] = true
			}
		}

		packageLocation := jsConfig.JSRoot
		if packageLocation == "." {
			packageLocation = ""
		}
		dataSet[fmt.Sprintf("//%s:package_json", packageLocation)] = true
	}

	deps := []string{}
	for dep := range depSet {
		deps = append(deps, dep)
	}
	if len(deps) > 0 {
		r.SetAttr("deps", deps)
	} else {
		r.DelAttr("deps")
	}

	data := []string{}
	for d := range dataSet {
		data = append(data, d)
	}
	if len(data) > 0 {
		r.SetAttr("data", data)
	} else {
		r.DelAttr("data")
	}
}

func (lang *JS) resolveWalkParents(name string, depSet map[string]bool, dataSet map[string]bool, c *config.Config, ix *resolve.RuleIndex, rc *repo.RemoteCache, r *rule.Rule, from label.Label) {

	jsConfigs := c.Exts[languageName].(JsConfigs)
	jsConfig := jsConfigs[from.Pkg]

	parents := ""
	tries := []string{}

	for {

		if name == "package" {
			name = "package.json"
		}
		if name == "." {
			name = "index"
		}

		localDir := path.Join(from.Pkg, parents)
		target := path.Join(localDir, name)

		// add supported extensions to target name to get a filePath
		extraExtensionsToTry := []string{""}
		if !lang.isWebAsset(jsConfig, target) {
			extraExtensionsToTry = append(append(extraExtensionsToTry, tsExtensions...), jsExtensions...)
		}

		for _, ext := range extraExtensionsToTry {

			filePath := target + ext
			tries = append(tries, filePath)

			// try to find a rule providing the filePath
			resolveResult := lang.tryResolve(filePath, c, ix, from)
			if resolveResult.err != nil {
				log.Print(Err("%v", resolveResult.err))
				return
			}
			if resolveResult.selfImport {
				// ignore self imports
				return
			}
			if resolveResult.label != label.NoLabel {
				// add discovered label
				lbl := resolveResult.label
				dep := lbl.Rel(from.Repo, from.Pkg).String()
				if !lang.isWebAsset(jsConfig, filePath) {
					depSet[dep] = true
				} else {
					dataSet[dep] = true
				}
				return
			}
			if resolveResult.fileName != "" {
				// add discovered file
				pkgName := path.Dir(target)
				data := fmt.Sprintf("//%s:%s", pkgName, resolveResult.fileName)
				dataSet[data] = true
				return
			}

		}

		if jsConfig.JSRoot == localDir || localDir == "." {
			// unable to resolve import
			if !jsConfig.Quiet {
				log.Print(Err("[%s] import %v not found", from.Abs(from.Repo, from.Pkg).String(), name))
			}
			if jsConfig.Verbose {
				log.Print(Warn("tried node_modules/%s", name))
				for _, try := range tries {
					log.Print(Warn("tried %s", try))
				}
			}
			return
		}

		// continue to search one directory higher
		parents += "../"
	}

}

// https://nodejs.org/api/modules.html#modules_all_together
func (lang *JS) isNpmDependency(imp string, jsConfig *JsConfig) (bool, string, bool) {

	// These prefixes cannot be NPM dependencies
	var prefixes = []string{".", "/", "../", "~/", "@/", "~~/"}
	if hasPrefix(prefixes, imp) {
		return false, "", false
	}

	// Grab the first part of the import (ie "foo/bar" -> "foo")
	packageRoot := imp
	for i := range imp {
		if imp[i] == '/' {
			prefix := imp[:i]
			if prefix == "@types" {
				continue
			} else {
				packageRoot = prefix
				break
			}
		}
	}

	// Is the package root found in package.json ?
	if npmLabel, ok := jsConfig.NpmDependencies.Dependencies[packageRoot]; ok {
		return true, npmLabel, false
	}

	if npmLabel, ok := jsConfig.NpmDependencies.DevDependencies[packageRoot]; ok {
		return true, npmLabel, true
	}

	// Assume all @ imports are npm dependencies
	if strings.HasPrefix(imp, "@types") {
		// Need to ignore @types, since these are checked greedily
		return false, "", false
	}
	if strings.HasPrefix(imp, "@") {
		return true, jsConfig.DefaultNpmLabel, false
	}

	return false, "", false
}

func hasPrefix(suffixes []string, x string) bool {
	for _, suffix := range suffixes {
		if strings.HasPrefix(x, suffix) {
			return true
		}
	}
	return false
}

type resolveResult struct {
	label      label.Label
	selfImport bool
	fileName   string
	err        error
}

func (lang *JS) tryResolve(target string, c *config.Config, ix *resolve.RuleIndex, from label.Label) resolveResult {

	importSpec := resolve.ImportSpec{
		Lang: lang.Name(),
		Imp:  target,
	}
	if override, ok := resolve.FindRuleWithOverride(c, importSpec, lang.Name()); ok {
		if override.Repo == "" {
			override.Repo = from.Repo
		}
		if !override.Equal(from) {
			if override.Repo == from.Repo {
				override.Repo = ""
			}
			return resolveResult{
				label:      override,
				selfImport: false,
				fileName:   "",
				err:        nil,
			}

		}
	}

	matches := ix.FindRulesByImportWithConfig(c, importSpec, lang.Name())

	// too many matches
	if len(matches) > 1 {
		return resolveResult{
			label:      label.NoLabel,
			selfImport: false,
			fileName:   "",
			err:        fmt.Errorf("multiple rules (%s and %s) provide %s", matches[0].Label, matches[1].Label, target),
		}
	}

	// no matches
	if len(matches) == 0 {

		// no rule is found for this file
		// it could be a regular file w/o a target
		if fileInfo, err := os.Stat(path.Join(c.RepoRoot, target)); err == nil && !fileInfo.IsDir() {
			// found a file matching the target
			return resolveResult{
				label:      label.NoLabel,
				selfImport: false,
				fileName:   fileInfo.Name(),
				err:        nil,
			}

		}
		return resolveResult{
			label:      label.NoLabel,
			selfImport: false,
			fileName:   "",
			err:        nil,
		}
	}

	if matches[0].IsSelfImport(from) {
		return resolveResult{
			label:      label.NoLabel,
			selfImport: true,
			fileName:   "",
			err:        nil,
		}
	}

	return resolveResult{
		label:      matches[0].Label,
		selfImport: false,
		fileName:   "",
		err:        nil,
	}

}
