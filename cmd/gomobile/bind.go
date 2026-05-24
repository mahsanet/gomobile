// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"

	"golang.org/x/mobile/internal/gobind"
	"golang.org/x/mobile/internal/sdkpath"
	"golang.org/x/mod/modfile"
	"golang.org/x/tools/go/packages"
)

var cmdBind = &command{
	run:   runBind,
	Name:  "bind",
	Usage: "[-target android|" + strings.Join(applePlatforms, "|") + "] [-bootclasspath <path>] [-classpath <path>] [-o output] [build flags] [package]",
	Short: "build a library for Android and iOS",
	Long: `
Bind generates language bindings for the package named by the import
path, and compiles a library for the named target system.

The -target flag takes either android (the default), or one or more
comma-delimited Apple platforms (` + strings.Join(applePlatforms, ", ") + `).

For -target android, the bind command produces an AAR (Android ARchive)
file that archives the precompiled Java API stub classes, the compiled
shared libraries, and all asset files in the /assets subdirectory under
the package directory. The output is named '<package_name>.aar' by
default. This AAR file is commonly used for binary distribution of an
Android library project and most Android IDEs support AAR import. For
example, in Android Studio (1.2+), an AAR file can be imported using
the module import wizard (File > New > New Module > Import .JAR or
.AAR package), and setting it as a new dependency
(File > Project Structure > Dependencies).  This requires 'javac'
(version 1.8+) and Android SDK (API level 16 or newer) to build the
library for Android. The ANDROID_HOME and ANDROID_NDK_HOME environment
variables can be used to specify the Android SDK and NDK if they are
not in the default locations. Use the -javapkg flag to specify the Java
package prefix for the generated classes.

By default, -target=android builds shared libraries for all supported
instruction sets (arm, arm64, 386, amd64). A subset of instruction sets
can be selected by specifying target type with the architecture name. E.g.,
-target=android/arm,android/386.

For Apple -target platforms, gomobile must be run on an OS X machine with
Xcode installed. The generated Objective-C types can be prefixed with the
-prefix flag.

For -target android, the -bootclasspath and -classpath flags are used to
control the bootstrap classpath and the classpath for Go wrappers to Java
classes.

The -v flag provides verbose output, including the list of packages built.

The build flags -a, -n, -x, -gcflags, -ldflags, -overlay, -tags, -trimpath,
and -work are shared with the build command. For documentation,
see 'go help build'.
`,
}

func runBind(cmd *command) error {
	bindGOPATH = ""
	bindModuleDir = ""
	cleanup, err := buildEnvInit()
	if err != nil {
		return err
	}
	defer cleanup()

	args := cmd.flag.Args()
	sonameExplicit := false
	cmd.flag.Visit(func(f *flag.Flag) {
		if f.Name == "soname" {
			sonameExplicit = true
		}
	})
	if !sonameExplicit && bindJavaPkg != "" {
		bindSoname = deriveBindSoname(bindJavaPkg)
	}
	if !validBindSoname(bindSoname) {
		return fmt.Errorf("invalid -soname %q: must match [a-zA-Z][a-zA-Z0-9_]*", bindSoname)
	}

	targets, err := parseBuildTarget(buildTarget)
	if err != nil {
		return fmt.Errorf(`invalid -target=%q: %v`, buildTarget, err)
	}

	if isAndroidPlatform(targets[0].platform) {
		if bindPrefix != "" {
			return fmt.Errorf("-prefix is supported only for Apple targets")
		}
		if _, err := ndkRoot(targets[0]); err != nil {
			return err
		}
	} else {
		if bindJavaPkg != "" {
			return fmt.Errorf("-javapkg is supported only for android target")
		}
	}

	if len(args) == 0 {
		args = append(args, ".")
	}
	if len(args) > 1 && !sonameExplicit && bindSoname == "gojni" {
		fmt.Fprintf(os.Stderr, "gomobile: warning: binding multiple packages with the default -soname %q may conflict with separately built AARs; prefer one bind invocation for all packages or pass explicit -soname values\n", bindSoname)
	}

	// TODO(ydnar): this should work, unless build tags affect loading a single package.
	// Should we try to import packages with different build tags per platform?
	pkgs, err := packages.Load(packagesConfig(targets[0]), args...)
	if err != nil {
		return err
	}
	for _, pkg := range pkgs {
		if pkg.Name == "" && len(pkg.Errors) > 0 {
			if err := setupExternalBindGOPATH(args); err != nil {
				return fmt.Errorf("%v", pkg.Errors)
			}
			pkgs, err = packages.Load(packagesConfig(targets[0]), args...)
			if err != nil {
				return err
			}
			break
		}
	}
	for _, pkg := range pkgs {
		if pkg.Name == "" && len(pkg.Errors) > 0 {
			return fmt.Errorf("%v", pkg.Errors)
		}
	}

	// check if any of the package is main
	for _, pkg := range pkgs {
		if pkg.Name == "main" {
			return fmt.Errorf(`binding "main" package (%s) is not supported`, pkg.PkgPath)
		}
	}

	switch {
	case isAndroidPlatform(targets[0].platform):
		return goAndroidBind(pkgs, targets)
	case isApplePlatform(targets[0].platform):
		if !xcodeAvailable() {
			return fmt.Errorf("-target=%q requires Xcode", buildTarget)
		}
		return goAppleBind(pkgs, targets)
	default:
		return fmt.Errorf(`invalid -target=%q`, buildTarget)
	}
}

var (
	bindPrefix        string // -prefix
	bindJavaPkg       string // -javapkg
	bindSoname        string // -soname
	bindClasspath     string // -classpath
	bindBootClasspath string // -bootclasspath
	bindGOPATH        string
	bindModuleDir     string
)

var bindSonameRE = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]*$`)

func validBindSoname(s string) bool {
	return bindSonameRE.MatchString(s)
}

func deriveBindSoname(javaPkg string) string {
	return "gojni_" + strings.ReplaceAll(javaPkg, ".", "_")
}

func init() {
	// bind command specific commands.
	cmdBind.flag.StringVar(&bindJavaPkg, "javapkg", "",
		"specifies custom Java package path prefix. Valid only with -target=android.")
	cmdBind.flag.StringVar(&bindSoname, "soname", "gojni",
		"name for the output shared library; produces lib<soname>.so and\n"+
			"namespaces all generated JNI symbols and runtime support classes.\n"+
			"If omitted with -javapkg, the name is derived as gojni_<javapkg with dots replaced by underscores>.\n"+
			"Must match [a-zA-Z][a-zA-Z0-9_]*")
	cmdBind.flag.StringVar(&bindPrefix, "prefix", "",
		"custom Objective-C name prefix. Valid only with -target=ios.")
	cmdBind.flag.StringVar(&bindClasspath, "classpath", "", "The classpath for imported Java classes. Valid only with -target=android.")
	cmdBind.flag.StringVar(&bindBootClasspath, "bootclasspath", "", "The bootstrap classpath for imported Java classes. Valid only with -target=android.")
}

func bootClasspath() (string, error) {
	if bindBootClasspath != "" {
		return bindBootClasspath, nil
	}
	apiPath, err := sdkpath.AndroidAPIPath(buildAndroidAPI)
	if err != nil {
		return "", err
	}
	return filepath.Join(apiPath, "android.jar"), nil
}

func runGobind(cfg gobind.Config, displayArgs []string) error {
	if buildX || buildN {
		dir := ""
		if cfg.Dir != "" {
			dir = "PWD=" + cfg.Dir + " "
		}
		env := strings.Join(cfg.Env, " ")
		if env != "" {
			env += " "
		}
		printcmd("%s%sgobind %s", dir, env, strings.Join(displayArgs, " "))
	}
	if buildN {
		if cfg.OutDir != "" {
			return writeGobindDryRunFiles(cfg)
		}
		return nil
	}
	cfg.MobilePkgPath = mobileModulePath()
	cfg.Stdout = os.Stdout
	cfg.Stderr = os.Stderr
	return gobind.Run(cfg)
}

func writeGobindDryRunFiles(cfg gobind.Config) error {
	for _, lang := range cfg.Langs {
		switch lang {
		case "objc":
			for _, arg := range cfg.Args {
				name := filepath.Base(arg)
				if name == "." || name == string(filepath.Separator) {
					name = ""
				}
				if name == "" {
					continue
				}
				title := cfg.Prefix + strings.Title(name)
				if err := writeGobindDryRunFile(cfg.OutDir, filepath.Join("src", "gobind", title+".objc.h")); err != nil {
					return err
				}
			}
			if err := writeGobindDryRunFile(cfg.OutDir, filepath.Join("src", "gobind", "Universe.objc.h")); err != nil {
				return err
			}
			if err := writeGobindDryRunFile(cfg.OutDir, filepath.Join("src", "gobind", "ref.h")); err != nil {
				return err
			}
		}
	}
	return nil
}

func writeGobindDryRunFile(outDir, name string) error {
	path := filepath.Join(outDir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.WriteFile(path, nil, 0644)
}

func copyFile(dst, src string) error {
	if buildX {
		printcmd("cp %s %s", src, dst)
	}
	return writeFile(dst, func(w io.Writer) error {
		if buildN {
			return nil
		}
		f, err := os.Open(src)
		if err != nil {
			return err
		}
		defer f.Close()

		if _, err := io.Copy(w, f); err != nil {
			return fmt.Errorf("cp %s %s failed: %v", src, dst, err)
		}
		return nil
	})
}

func writeFile(filename string, generate func(io.Writer) error) error {
	if buildV {
		fmt.Fprintf(os.Stderr, "write %s\n", filename)
	}

	if err := mkdir(filepath.Dir(filename)); err != nil {
		return err
	}

	if buildN {
		return generate(io.Discard)
	}

	f, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()

	return generate(f)
}

func packagesConfig(t targetInfo) *packages.Config {
	config := &packages.Config{}
	config.Dir = bindModuleDir
	// Add CGO_ENABLED=1 explicitly since Cgo is disabled when GOOS is different from host OS.
	config.Env = append(os.Environ(), "GOARCH="+t.arch, "GOOS="+platformOS(t.platform), "CGO_ENABLED=1")
	config.Env = append(config.Env, bindEnv()...)
	tags := append(buildTags[:], platformTags(t.platform)...)

	if len(tags) > 0 {
		config.BuildFlags = []string{"-tags=" + strings.Join(tags, ",")}
	}
	if bindVendorDir() != "" {
		config.BuildFlags = append(config.BuildFlags, "-mod=vendor")
	} else if bindModuleDir != "" {
		config.BuildFlags = append(config.BuildFlags, "-mod=mod")
	}
	return config
}

func bindEnv() []string {
	if bindGOPATH == "" {
		return nil
	}
	return []string{
		"GO111MODULE=off",
		"GOPATH=" + bindGOPATH + string(filepath.ListSeparator) + goEnv("GOPATH"),
	}
}

func setupExternalBindGOPATH(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("no packages")
	}
	gopath := filepath.Join(tmpdir, "bind-gopath")
	for _, arg := range args {
		if !isRemoteImportPath(arg) {
			return fmt.Errorf("package %q is not a remote import path", arg)
		}
		mod, err := moduleForImportPath(arg)
		if err != nil {
			return err
		}
		if _, err := os.Stat(filepath.Join(mod.Dir, "go.mod")); err == nil {
			bindModuleDir = mod.Dir
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := symlinkGOPATHPackage(gopath, mod.Path, mod.Dir); err != nil {
			return err
		}
	}
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	if err := symlinkGOPATHPackage(gopath, mobileModulePath(), wd); err != nil {
		return err
	}
	bindGOPATH = gopath
	return nil
}

func isRemoteImportPath(path string) bool {
	if path == "" || strings.HasPrefix(path, ".") || filepath.IsAbs(path) {
		return false
	}
	first, _, _ := strings.Cut(path, "/")
	return strings.Contains(first, ".")
}

type moduleJSON struct {
	Path    string
	Version string
	Dir     string
	Origin  *moduleOrigin
}

type moduleOrigin struct {
	URL  string
	Hash string
}

func moduleForImportPath(path string) (*moduleJSON, error) {
	parts := strings.Split(path, "/")
	for n := len(parts); n > 0; n-- {
		modPath := strings.Join(parts[:n], "/")
		for _, query := range []string{"master", "main", "latest"} {
			mod, err := queryModule(modPath, query)
			if err == nil {
				return mod, nil
			}
		}
	}
	return nil, fmt.Errorf("cannot resolve module for package %q", path)
}

func queryModule(path, query string) (*moduleJSON, error) {
	cmd := exec.Command("go", "list", "-m", "-json", path+"@"+query)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var mod moduleJSON
	if err := json.Unmarshal(out, &mod); err != nil {
		return nil, err
	}
	if mod.Path == "" {
		return nil, fmt.Errorf("module path is empty")
	}
	if mod.Origin != nil && mod.Origin.URL != "" && mod.Origin.Hash != "" && query != "latest" {
		if dir, err := cloneModuleSource(mod.Path, mod.Origin.URL, mod.Origin.Hash); err == nil {
			mod.Dir = dir
			return &mod, nil
		}
	}
	if mod.Dir == "" {
		mod, err = downloadModule(mod.Path, mod.Version)
		if err != nil {
			return nil, err
		}
	}
	if mod.Dir == "" {
		return nil, fmt.Errorf("module dir is empty")
	}
	return &mod, nil
}

func cloneModuleSource(path, url, hash string) (string, error) {
	dir := filepath.Join(tmpdir, "bind-module", filepath.FromSlash(path))
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		return dir, nil
	} else if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	if err := mkdir(filepath.Dir(dir)); err != nil {
		return "", err
	}
	cmd := exec.Command("git", "clone", "--recursive", url, dir)
	if err := runCmd(cmd); err != nil {
		return "", err
	}
	cmd = exec.Command("git", "checkout", hash)
	cmd.Dir = dir
	if err := runCmd(cmd); err != nil {
		return "", err
	}
	cmd = exec.Command("git", "submodule", "update", "--init", "--recursive")
	cmd.Dir = dir
	if err := runCmd(cmd); err != nil {
		return "", err
	}
	return dir, nil
}

func downloadModule(path, version string) (moduleJSON, error) {
	var mod moduleJSON
	if version == "" {
		return mod, fmt.Errorf("module version is empty")
	}
	cmd := exec.Command("go", "mod", "download", "-json", path+"@"+version)
	out, err := cmd.Output()
	if err != nil {
		return mod, err
	}
	if err := json.Unmarshal(out, &mod); err != nil {
		return mod, err
	}
	return mod, nil
}

func symlinkGOPATHPackage(gopath, importPath, dir string) error {
	dst := filepath.Join(gopath, "src", filepath.FromSlash(importPath))
	if _, err := os.Stat(dst); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := mkdir(filepath.Dir(dst)); err != nil {
		return err
	}
	return symlink(dir, dst)
}

func mobileModulePath() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "golang.org/x/mobile"
	}
	path := info.Main.Path
	if strings.HasSuffix(path, "/cmd/gomobile") {
		return strings.TrimSuffix(path, "/cmd/gomobile")
	}
	return "golang.org/x/mobile"
}

// getModuleVersions returns a module information at the directory src.
func getModuleVersions(targetPlatform string, targetArch string, src string) (*modfile.File, error) {
	cmd := exec.Command("go", "list")
	cmd.Env = append(os.Environ(), "GOOS="+platformOS(targetPlatform), "GOARCH="+targetArch)

	tags := append(buildTags[:], platformTags(targetPlatform)...)

	// TODO(hyangah): probably we don't need to add all the dependencies.
	cmd.Args = append(cmd.Args, "-m", "-json", "-tags="+strings.Join(tags, ","), "-mod=mod", "all")
	cmd.Dir = src

	output, err := cmd.Output()
	if err != nil {
		// Module information is not available at src.
		return nil, nil
	}

	type Module struct {
		Main    bool
		Path    string
		Version string
		Dir     string
		Replace *Module
	}

	f := &modfile.File{}
	if err := f.AddModuleStmt("gobind"); err != nil {
		return nil, err
	}
	e := json.NewDecoder(bytes.NewReader(output))
	for {
		var mod *Module
		err := e.Decode(&mod)
		if err != nil && err != io.EOF {
			return nil, err
		}
		if mod != nil {
			if mod.Replace != nil {
				p, v := mod.Replace.Path, mod.Replace.Version
				if modfile.IsDirectoryPath(p) {
					// replaced by a local directory
					p = mod.Replace.Dir
				}
				if err := f.AddReplace(mod.Path, mod.Version, p, v); err != nil {
					return nil, err
				}
			} else {
				// When the version part is empty, the module is local and mod.Dir represents the location.
				if v := mod.Version; v == "" {
					if err := f.AddReplace(mod.Path, mod.Version, mod.Dir, ""); err != nil {
						return nil, err
					}
				} else {
					if err := f.AddRequire(mod.Path, v); err != nil {
						return nil, err
					}
				}
			}
		}
		if err == io.EOF {
			break
		}
	}

	v, err := ensureGoVersion()
	if err != nil {
		return nil, err
	}
	// ensureGoVersion can return an empty string for a devel version. In this case, use the minimum version.
	if v == "" {
		v = fmt.Sprintf("go1.%d", minimumGoMinorVersion)
	}
	if err := f.AddGoStmt(strings.TrimPrefix(v, "go")); err != nil {
		return nil, err
	}
	return f, nil
}

// writeGoMod writes go.mod file at dir when Go modules are used.
func writeGoMod(dir, targetPlatform, targetArch string) error {
	m, err := areGoModulesUsed()
	if err != nil {
		return err
	}
	// If Go modules are not used, go.mod should not be created because the dependencies might not be compatible with Go modules.
	if !m {
		return nil
	}

	return writeFile(filepath.Join(dir, "go.mod"), func(w io.Writer) error {
		src := "."
		if bindModuleDir != "" {
			src = bindModuleDir
		}
		f, err := getModuleVersions(targetPlatform, targetArch, src)
		if err != nil {
			return err
		}
		if f == nil {
			return nil
		}
		bs, err := f.Format()
		if err != nil {
			return err
		}
		if _, err := w.Write(bs); err != nil {
			return err
		}
		return nil
	})
}

func bindVendorDir() string {
	dir := bindModuleDir
	if dir == "" {
		cmd := exec.Command("go", "env", "GOMOD")
		out, err := cmd.Output()
		if err != nil {
			return ""
		}
		gomod := strings.TrimSpace(string(out))
		if gomod == "" || gomod == os.DevNull {
			return ""
		}
		dir = filepath.Dir(gomod)
	}
	if _, err := os.Stat(filepath.Join(dir, "vendor", "modules.txt")); err != nil {
		return ""
	}
	return dir
}

func copyBindModuleForVendor(dst string) error {
	src := bindVendorDir()
	if src == "" {
		return fmt.Errorf("vendor directory is not available")
	}
	if err := copyBindDir(dst, src, func(rel string, info os.FileInfo) bool {
		return rel == ".git" || strings.HasPrefix(rel, ".git"+string(filepath.Separator))
	}); err != nil {
		return err
	}
	return vendorMobileModule(dst)
}

func copyBindGeneratedSource(dst, src string) error {
	return copyBindDir(dst, src, nil)
}

func copyBindDir(dst, src string, skip func(rel string, info os.FileInfo) bool) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, errin error) (err error) {
		if errin != nil {
			return errin
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(dst, 0755)
		}
		if skip != nil && skip(rel, info) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		outpath := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(outpath, 0755)
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(outpath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode())
		if err != nil {
			return err
		}
		defer func() {
			if errc := out.Close(); err == nil {
				err = errc
			}
		}()
		_, err = io.Copy(out, in)
		return err
	})
}

func vendorMobileModule(moduleDir string) error {
	modPath := mobileModulePath()
	modulesTxt := filepath.Join(moduleDir, "vendor", "modules.txt")
	b, err := os.ReadFile(modulesTxt)
	if err != nil {
		return err
	}
	if bytes.Contains(b, []byte("# "+modPath+" ")) {
		return nil
	}

	modDir, modVersion, err := mobileModuleSource()
	if err != nil {
		return err
	}
	if modDir == "" {
		return fmt.Errorf("module %s has no source directory", modPath)
	}
	vendorDir := filepath.Join(moduleDir, "vendor", filepath.FromSlash(modPath))
	if err := copyBindDir(vendorDir, modDir, func(rel string, info os.FileInfo) bool {
		return rel == ".git" ||
			strings.HasPrefix(rel, ".git"+string(filepath.Separator)) ||
			rel == "vendor" ||
			strings.HasPrefix(rel, "vendor"+string(filepath.Separator))
	}); err != nil {
		return err
	}
	if err := addMobileRequire(moduleDir, modPath, modVersion); err != nil {
		return err
	}
	pkgs, err := vendorPackageList(vendorDir, modPath)
	if err != nil {
		return err
	}
	goVersion := vendorModuleGoVersion(modDir)
	var entry strings.Builder
	fmt.Fprintf(&entry, "\n# %s %s\n", modPath, modVersion)
	if goVersion == "" {
		fmt.Fprintln(&entry, "## explicit")
	} else {
		fmt.Fprintf(&entry, "## explicit; go %s\n", goVersion)
	}
	for _, p := range pkgs {
		fmt.Fprintln(&entry, p)
	}
	f, err := os.OpenFile(modulesTxt, os.O_APPEND|os.O_WRONLY, 0666)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(entry.String())
	return err
}

func mobileModuleSource() (dir, version string, err error) {
	if _, file, _, ok := runtime.Caller(0); ok {
		dir = filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
		if modulePathAt(dir) == mobileModulePath() {
			version = mobileModuleVersion()
			if version == "latest" {
				version = "v0.0.0"
			}
			return dir, version, nil
		}
	}
	mod, err := downloadModule(mobileModulePath(), mobileModuleVersion())
	if err != nil {
		return "", "", err
	}
	return mod.Dir, mod.Version, nil
}

func mobileModuleVersion() string {
	info, ok := debug.ReadBuildInfo()
	if ok {
		if info.Main.Path == mobileModulePath() && info.Main.Version != "" && info.Main.Version != "(devel)" {
			return info.Main.Version
		}
		for _, dep := range info.Deps {
			if dep.Path == mobileModulePath() && dep.Version != "" {
				return dep.Version
			}
		}
	}
	return "latest"
}

func modulePathAt(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return ""
	}
	f, err := modfile.Parse("go.mod", b, nil)
	if err != nil || f.Module == nil {
		return ""
	}
	return f.Module.Mod.Path
}

func addMobileRequire(moduleDir, path, version string) error {
	gomod := filepath.Join(moduleDir, "go.mod")
	b, err := os.ReadFile(gomod)
	if err != nil {
		return err
	}
	f, err := modfile.Parse(gomod, b, nil)
	if err != nil {
		return err
	}
	if err := f.AddRequire(path, version); err != nil {
		return err
	}
	out, err := f.Format()
	if err != nil {
		return err
	}
	return os.WriteFile(gomod, out, 0666)
}

func vendorModuleGoVersion(moduleDir string) string {
	b, err := os.ReadFile(filepath.Join(moduleDir, "go.mod"))
	if err != nil {
		return ""
	}
	f, err := modfile.Parse("go.mod", b, nil)
	if err != nil || f.Go == nil {
		return ""
	}
	return f.Go.Version
}

func vendorPackageList(moduleDir, modulePath string) ([]string, error) {
	var pkgs []string
	err := filepath.Walk(moduleDir, func(path string, info os.FileInfo, errin error) error {
		if errin != nil {
			return errin
		}
		if !info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(moduleDir, path)
		if err != nil {
			return err
		}
		if rel != "." && strings.HasPrefix(filepath.Base(rel), ".") {
			return filepath.SkipDir
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
				continue
			}
			if rel == "." {
				pkgs = append(pkgs, modulePath)
			} else {
				pkgs = append(pkgs, modulePath+"/"+filepath.ToSlash(rel))
			}
			break
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return pkgs, nil
}

var (
	areGoModulesUsedResult struct {
		used bool
		err  error
	}
	areGoModulesUsedOnce sync.Once
)

func areGoModulesUsed() (bool, error) {
	if bindGOPATH != "" {
		return false, nil
	}
	if bindModuleDir != "" {
		return true, nil
	}
	areGoModulesUsedOnce.Do(func() {
		out, err := exec.Command("go", "env", "GOMOD").Output()
		if err != nil {
			areGoModulesUsedResult.err = err
			return
		}
		outstr := strings.TrimSpace(string(out))
		areGoModulesUsedResult.used = outstr != ""
	})
	return areGoModulesUsedResult.used, areGoModulesUsedResult.err
}
