// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gobind

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
	"text/template"
	"unicode"
	"unicode/utf8"

	"golang.org/x/mobile/bind"
	"golang.org/x/mobile/internal/importers"
	"golang.org/x/mobile/internal/importers/java"
	"golang.org/x/mobile/internal/importers/objc"
	"golang.org/x/tools/go/packages"
)

const defaultMobilePkgPath = "golang.org/x/mobile"

// Config describes a gobind generation run.
type Config struct {
	Langs         []string
	OutDir        string
	JavaPkg       string
	Soname        string
	Prefix        string
	Bootclasspath string
	Classpath     string
	Tags          []string
	Args          []string
	Dir           string
	Env           []string
	Stdout        io.Writer
	Stderr        io.Writer
	MobilePkgPath string
}

var sonameRE = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]*$`)

// ValidSoname reports whether s is valid as an Android shared library base name.
func ValidSoname(s string) bool {
	return sonameRE.MatchString(s)
}

// Run generates bindings according to cfg.
func Run(cfg Config) error {
	if cfg.Soname == "" {
		cfg.Soname = "gojni"
	}
	if !ValidSoname(cfg.Soname) {
		return fmt.Errorf("invalid -soname %q: must match [a-zA-Z][a-zA-Z0-9_]*", cfg.Soname)
	}
	if len(cfg.Langs) == 0 {
		cfg.Langs = []string{"go", "java", "objc"}
	}
	if cfg.Stdout == nil {
		cfg.Stdout = os.Stdout
	}
	if cfg.Stderr == nil {
		cfg.Stderr = os.Stderr
	}
	if cfg.MobilePkgPath == "" {
		cfg.MobilePkgPath = mobilePkgPath()
	}
	if cfg.OutDir != "" {
		abs, err := filepath.Abs(cfg.OutDir)
		if err != nil {
			return err
		}
		cfg.OutDir = abs
	}

	g := &generator{
		cfg:  cfg,
		fset: token.NewFileSet(),
	}
	return g.run()
}

type generator struct {
	cfg  Config
	fset *token.FileSet
}

func (g *generator) run() error {
	// We need to give appropriate environment variables like CC or CXX so that the returned packages no longer have errors.
	// However, getting such environment variables is difficult or impossible so far.
	// Gomobile can obtain such environment variables in env.go, but this logic assumes some conditions gobind doesn't assume.
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles |
			packages.NeedImports | packages.NeedDeps |
			packages.NeedTypes | packages.NeedSyntax | packages.NeedTypesInfo,
		BuildFlags: []string{"-tags", strings.Join(g.cfg.Tags, " ")},
		Dir:        g.cfg.Dir,
		Env:        mergedEnv(g.cfg.Env),
	}

	// Call Load twice to warm the cache. There is a known issue that the result of Load
	// depends on build cache state. See golang/go#33687.
	packages.Load(cfg, g.cfg.Args...)

	allPkg, err := packages.Load(cfg, g.cfg.Args...)
	if err != nil {
		return err
	}

	jrefs, err := importers.AnalyzePackages(allPkg, "Java/")
	if err != nil {
		return err
	}
	orefs, err := importers.AnalyzePackages(allPkg, "ObjC/")
	if err != nil {
		return err
	}
	var classes []*java.Class
	if len(jrefs.Refs) > 0 {
		jimp := &java.Importer{
			Bootclasspath: g.cfg.Bootclasspath,
			Classpath:     g.cfg.Classpath,
			JavaPkg:       g.cfg.JavaPkg,
		}
		classes, err = jimp.Import(jrefs)
		if err != nil {
			return err
		}
	}
	var otypes []*objc.Named
	if len(orefs.Refs) > 0 {
		otypes, err = objc.Import(orefs)
		if err != nil {
			return err
		}
	}

	if len(classes) > 0 || len(otypes) > 0 {
		srcDir := g.cfg.OutDir
		if srcDir == "" {
			srcDir, err = os.MkdirTemp(os.TempDir(), "gobind-")
			if err != nil {
				return err
			}
			defer os.RemoveAll(srcDir)
		}
		if len(classes) > 0 {
			if err := g.genJavaPackages(srcDir, classes, jrefs.Embedders); err != nil {
				return err
			}
		}
		if len(otypes) > 0 {
			if err := g.genObjcPackages(srcDir, otypes, orefs.Embedders); err != nil {
				return err
			}
		}

		// Add a new directory to GOPATH where the file for reverse bindings exist, and recreate allPkg.
		// It is because the current allPkg did not solve imports for reverse bindings.
		gopath, err := g.goEnv("GOPATH")
		if err != nil {
			return err
		}
		if gopath != "" {
			gopath = string(filepath.ListSeparator) + gopath
		}
		gopath = srcDir + gopath
		cfg.Env = append(mergedEnv(g.cfg.Env), "GOPATH="+gopath)
		allPkg, err = packages.Load(cfg, g.cfg.Args...)
		if err != nil {
			return err
		}
	}

	typePkgs := make([]*types.Package, len(allPkg))
	astPkgs := make([][]*ast.File, len(allPkg))
	for i, pkg := range allPkg {
		// Ignore pkg.Errors. pkg.Errors can exist when Cgo is used, but this should not affect the result.
		// See the discussion at golang/go#36547.
		typePkgs[i] = pkg.Types
		astPkgs[i] = pkg.Syntax
	}
	for _, l := range g.cfg.Langs {
		for i, pkg := range typePkgs {
			if err := g.genPkg(l, pkg, astPkgs[i], typePkgs, classes, otypes); err != nil {
				return err
			}
		}
		// Generate the error package and support files.
		if err := g.genPkg(l, nil, nil, typePkgs, classes, otypes); err != nil {
			return err
		}
	}
	return nil
}

func (g *generator) goEnv(name string) (string, error) {
	cmd := exec.Command("go", "env", name)
	cmd.Dir = g.cfg.Dir
	cmd.Env = mergedEnv(g.cfg.Env)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func mergedEnv(overrides []string) []string {
	env := os.Environ()
	index := make(map[string]int, len(env))
	for i, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		index[key] = i
	}
	for _, kv := range overrides {
		key, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if i, ok := index[key]; ok {
			env[i] = kv
		} else {
			index[key] = len(env)
			env = append(env, kv)
		}
	}
	return env
}

func mobilePkgPath() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return defaultMobilePkgPath
	}
	path := info.Main.Path
	if strings.HasSuffix(path, "/cmd/gobind") {
		return strings.TrimSuffix(path, "/cmd/gobind")
	}
	if strings.HasSuffix(path, "/cmd/gomobile") {
		return strings.TrimSuffix(path, "/cmd/gomobile")
	}
	return defaultMobilePkgPath
}

func (g *generator) genPkg(lang string, p *types.Package, astFiles []*ast.File, allPkg []*types.Package, classes []*java.Class, otypes []*objc.Named) error {
	fname, err := g.defaultFileName(lang, p)
	if err != nil {
		return err
	}
	conf := &bind.GeneratorConfig{
		Fset:          g.fset,
		Pkg:           p,
		AllPkg:        allPkg,
		MobilePkgPath: g.cfg.MobilePkgPath,
	}
	var pname string
	if p != nil {
		pname = p.Name()
	} else {
		pname = "universe"
	}
	var buf bytes.Buffer
	bgen := &bind.Generator{
		Printer: &bind.Printer{Buf: &buf, IndentEach: []byte("\t")},
		Fset:    conf.Fset,
		AllPkg:  conf.AllPkg,
		Pkg:     conf.Pkg,
		Files:   astFiles,
	}
	switch lang {
	case "java":
		jg := &bind.JavaGen{
			JavaPkg:   g.cfg.JavaPkg,
			SeqPkg:    g.seqPkgName(),
			Soname:    g.cfg.Soname,
			Generator: bgen,
		}
		jg.Init(classes)

		pkgname := bind.JavaPkgName(g.cfg.JavaPkg, p)
		if p == nil {
			pkgname = g.seqPkgName()
		}
		pkgDir := strings.Replace(pkgname, ".", "/", -1)
		buf.Reset()
		w, closer, err := g.writer(filepath.Join("java", pkgDir, fname))
		if err != nil {
			return err
		}
		if err := processErr(jg.GenJava()); err != nil {
			closer()
			return err
		}
		if _, err := io.Copy(w, &buf); err != nil {
			closer()
			return err
		}
		if err := closer(); err != nil {
			return err
		}
		for i, name := range jg.ClassNames() {
			buf.Reset()
			w, closer, err := g.writer(filepath.Join("java", pkgDir, name+".java"))
			if err != nil {
				return err
			}
			if err := processErr(jg.GenClass(i)); err != nil {
				closer()
				return err
			}
			if _, err := io.Copy(w, &buf); err != nil {
				closer()
				return err
			}
			if err := closer(); err != nil {
				return err
			}
		}
		buf.Reset()
		w, closer, err = g.writer(filepath.Join("src", "gobind", pname+"_android.c"))
		if err != nil {
			return err
		}
		if err := processErr(jg.GenC()); err != nil {
			closer()
			return err
		}
		if _, err := io.Copy(w, &buf); err != nil {
			closer()
			return err
		}
		if err := closer(); err != nil {
			return err
		}
		buf.Reset()
		w, closer, err = g.writer(filepath.Join("src", "gobind", pname+"_android.h"))
		if err != nil {
			return err
		}
		if err := processErr(jg.GenH()); err != nil {
			closer()
			return err
		}
		if _, err := io.Copy(w, &buf); err != nil {
			closer()
			return err
		}
		if err := closer(); err != nil {
			return err
		}
		// Generate support files along with the universe package.
		if p == nil {
			for _, javaFile := range []string{"Seq.java"} {
				w, closer, err := g.writer(filepath.Join("java", filepath.FromSlash(strings.ReplaceAll(g.seqPkgName(), ".", "/")), javaFile))
				if err != nil {
					return err
				}
				if err := renderSupportTemplate(w, bind.SupportFiles, "java/"+javaFile, g.supportTemplateData()); err != nil {
					closer()
					return fmt.Errorf("failed to render Java support file: %v", err)
				}
				if err := closer(); err != nil {
					return err
				}
			}
			// Copy support files.
			if err := g.renderFile(filepath.Join("src", "gobind", "seq_android.c"), "java/seq_android.c.support", g.supportTemplateData()); err != nil {
				return err
			}
			if err := g.renderFile(filepath.Join("src", "gobind", "seq_android.go"), "java/seq_android.go.support", g.supportTemplateData()); err != nil {
				return err
			}
			if err := g.renderFile(filepath.Join("src", "gobind", "seq_android.h"), "java/seq_android.h", g.supportTemplateData()); err != nil {
				return err
			}
		}
	case "go":
		w, closer, err := g.writer(filepath.Join("src", "gobind", fname))
		if err != nil {
			return err
		}
		conf.Writer = w
		if err := processErr(bind.GenGo(conf)); err != nil {
			closer()
			return err
		}
		if err := closer(); err != nil {
			return err
		}
		w, closer, err = g.writer(filepath.Join("src", "gobind", pname+".h"))
		if err != nil {
			return err
		}
		genPkgH(w, pname)
		if _, err := io.Copy(w, &buf); err != nil {
			closer()
			return err
		}
		if err := closer(); err != nil {
			return err
		}
		w, closer, err = g.writer(filepath.Join("src", "gobind", "seq.h"))
		if err != nil {
			return err
		}
		genPkgH(w, "seq")
		if _, err := io.Copy(w, &buf); err != nil {
			closer()
			return err
		}
		if err := closer(); err != nil {
			return err
		}
		if err := g.renderFile(filepath.Join("src", "gobind", "seq.go"), "seq.go.support", g.supportTemplateData()); err != nil {
			return err
		}
	case "objc":
		og := &bind.ObjcGen{
			Generator: bgen,
			Prefix:    g.cfg.Prefix,
		}
		og.Init(otypes)
		w, closer, err := g.writer(filepath.Join("src", "gobind", pname+"_darwin.h"))
		if err != nil {
			return err
		}
		if err := processErr(og.GenGoH()); err != nil {
			closer()
			return err
		}
		if _, err := io.Copy(w, &buf); err != nil {
			closer()
			return err
		}
		if err := closer(); err != nil {
			return err
		}
		hname := strings.Title(fname[:len(fname)-2]) + ".objc.h"
		w, closer, err = g.writer(filepath.Join("src", "gobind", hname))
		if err != nil {
			return err
		}
		if err := processErr(og.GenH()); err != nil {
			closer()
			return err
		}
		if _, err := io.Copy(w, &buf); err != nil {
			closer()
			return err
		}
		if err := closer(); err != nil {
			return err
		}
		mname := strings.Title(fname[:len(fname)-2]) + "_darwin.m"
		w, closer, err = g.writer(filepath.Join("src", "gobind", mname))
		if err != nil {
			return err
		}
		conf.Writer = w
		if err := processErr(og.GenM()); err != nil {
			closer()
			return err
		}
		if _, err := io.Copy(w, &buf); err != nil {
			closer()
			return err
		}
		if err := closer(); err != nil {
			return err
		}
		if p == nil {
			// Copy support files.
			if err := g.copyFile(filepath.Join("src", "gobind", "seq_darwin.m"), "objc/seq_darwin.m.support"); err != nil {
				return err
			}
			if err := g.copyFile(filepath.Join("src", "gobind", "seq_darwin.go"), "objc/seq_darwin.go.support"); err != nil {
				return err
			}
			if err := g.copyFile(filepath.Join("src", "gobind", "ref.h"), "objc/ref.h"); err != nil {
				return err
			}
			if err := g.copyFile(filepath.Join("src", "gobind", "seq_darwin.h"), "objc/seq_darwin.h"); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unknown target language: %q", lang)
	}
	return nil
}

type supportData struct {
	MobilePkg   string
	SeqPkg      string
	SeqPkgSlash string
	SeqPkgJNI   string
	Soname      string
}

func (g *generator) seqPkgName() string {
	if g.cfg.JavaPkg != "" {
		return g.cfg.JavaPkg + ".go"
	}
	if g.cfg.Soname != "" && g.cfg.Soname != "gojni" {
		return "go." + g.cfg.Soname
	}
	return "go"
}

func (g *generator) supportTemplateData() supportData {
	seqPkg := g.seqPkgName()
	return supportData{
		MobilePkg:   g.cfg.MobilePkgPath,
		SeqPkg:      seqPkg,
		SeqPkgSlash: strings.ReplaceAll(seqPkg, ".", "/"),
		SeqPkgJNI:   strings.ReplaceAll(java.JNIMangle(seqPkg), ".", "_"),
		Soname:      g.cfg.Soname,
	}
}

func (g *generator) renderFile(dst, src string, data supportData) error {
	w, closer, err := g.writer(dst)
	if err != nil {
		return err
	}
	if err := renderSupportTemplate(w, bind.SupportFiles, src, data); err != nil {
		closer()
		return fmt.Errorf("failed to render support file: %v", err)
	}
	return closer()
}

func renderSupportTemplate(w io.Writer, files fs.FS, src string, data supportData) error {
	tmpl, err := template.ParseFS(files, src)
	if err != nil {
		return err
	}
	return tmpl.Execute(w, data)
}

func genPkgH(w io.Writer, pname string) {
	fmt.Fprintf(w, `// Code generated by gobind. DO NOT EDIT.

#ifdef __GOBIND_ANDROID__
#include "%[1]s_android.h"
#endif
#ifdef __GOBIND_DARWIN__
#include "%[1]s_darwin.h"
#endif`, pname)
}

func (g *generator) genObjcPackages(dir string, types []*objc.Named, embedders []importers.Struct) error {
	var buf bytes.Buffer
	cg := &bind.ObjcWrapper{
		Printer: &bind.Printer{
			IndentEach: []byte("\t"),
			Buf:        &buf,
		},
	}
	var genNames []string
	for _, emb := range embedders {
		genNames = append(genNames, emb.Name)
	}
	cg.Init(types, genNames)
	for i, opkg := range cg.Packages() {
		pkgDir := filepath.Join(dir, "src", "ObjC", opkg)
		if err := os.MkdirAll(pkgDir, 0700); err != nil {
			return err
		}
		pkgFile := filepath.Join(pkgDir, "package.go")
		buf.Reset()
		cg.GenPackage(i)
		if err := os.WriteFile(pkgFile, buf.Bytes(), 0600); err != nil {
			return err
		}
	}
	buf.Reset()
	cg.GenInterfaces()
	objcBase := filepath.Join(dir, "src", "ObjC")
	if err := os.MkdirAll(objcBase, 0700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(objcBase, "interfaces.go"), buf.Bytes(), 0600); err != nil {
		return err
	}
	goBase := filepath.Join(dir, "src", "gobind")
	if err := os.MkdirAll(goBase, 0700); err != nil {
		return err
	}
	buf.Reset()
	cg.GenGo()
	if err := os.WriteFile(filepath.Join(goBase, "interfaces_darwin.go"), buf.Bytes(), 0600); err != nil {
		return err
	}
	buf.Reset()
	cg.GenH()
	if err := os.WriteFile(filepath.Join(goBase, "interfaces.h"), buf.Bytes(), 0600); err != nil {
		return err
	}
	buf.Reset()
	cg.GenM()
	if err := os.WriteFile(filepath.Join(goBase, "interfaces_darwin.m"), buf.Bytes(), 0600); err != nil {
		return err
	}
	return nil
}

func (g *generator) genJavaPackages(dir string, classes []*java.Class, embedders []importers.Struct) error {
	var buf bytes.Buffer
	cg := &bind.ClassGen{
		JavaPkg: g.cfg.JavaPkg,
		Printer: &bind.Printer{
			IndentEach: []byte("\t"),
			Buf:        &buf,
		},
	}
	cg.Init(classes, embedders)
	for i, jpkg := range cg.Packages() {
		pkgDir := filepath.Join(dir, "src", "Java", jpkg)
		if err := os.MkdirAll(pkgDir, 0700); err != nil {
			return err
		}
		pkgFile := filepath.Join(pkgDir, "package.go")
		buf.Reset()
		cg.GenPackage(i)
		if err := os.WriteFile(pkgFile, buf.Bytes(), 0600); err != nil {
			return err
		}
	}
	buf.Reset()
	cg.GenInterfaces()
	javaBase := filepath.Join(dir, "src", "Java")
	if err := os.MkdirAll(javaBase, 0700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(javaBase, "interfaces.go"), buf.Bytes(), 0600); err != nil {
		return err
	}
	goBase := filepath.Join(dir, "src", "gobind")
	if err := os.MkdirAll(goBase, 0700); err != nil {
		return err
	}
	buf.Reset()
	cg.GenGo()
	if err := os.WriteFile(filepath.Join(goBase, "classes_android.go"), buf.Bytes(), 0600); err != nil {
		return err
	}
	buf.Reset()
	cg.GenH()
	if err := os.WriteFile(filepath.Join(goBase, "classes.h"), buf.Bytes(), 0600); err != nil {
		return err
	}
	buf.Reset()
	cg.GenC()
	if err := os.WriteFile(filepath.Join(goBase, "classes_android.c"), buf.Bytes(), 0600); err != nil {
		return err
	}
	return nil
}

func processErr(err error) error {
	if err == nil {
		return nil
	}
	if list, _ := err.(bind.ErrorList); len(list) > 0 {
		var b strings.Builder
		for _, err := range list {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(err.Error())
		}
		return fmt.Errorf("%s", b.String())
	}
	return err
}

func (g *generator) writer(fname string) (w io.Writer, closer func() error, err error) {
	if g.cfg.OutDir == "" {
		return g.cfg.Stdout, func() error { return nil }, nil
	}

	name := filepath.Join(g.cfg.OutDir, fname)
	dir := filepath.Dir(name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, nil, fmt.Errorf("invalid output dir: %v", err)
	}

	f, err := os.Create(name)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid output dir: %v", err)
	}
	return f, f.Close, nil
}

func (g *generator) copyFile(dst, src string) error {
	w, closer, err := g.writer(dst)
	if err != nil {
		return err
	}
	f, err := bind.SupportFiles.Open(src)
	if err != nil {
		closer()
		return fmt.Errorf("unable to open file: %v", err)
	}
	defer f.Close()
	if _, err := io.Copy(w, f); err != nil {
		closer()
		return fmt.Errorf("unable to copy file: %v", err)
	}
	return closer()
}

func (g *generator) defaultFileName(lang string, pkg *types.Package) (string, error) {
	switch lang {
	case "java":
		if pkg == nil {
			return "Universe.java", nil
		}
		firstRune, size := utf8.DecodeRuneInString(pkg.Name())
		className := string(unicode.ToUpper(firstRune)) + pkg.Name()[size:]
		return className + ".java", nil
	case "go":
		if pkg == nil {
			return "go_main.go", nil
		}
		return "go_" + pkg.Name() + "main.go", nil
	case "objc":
		if pkg == nil {
			return "Universe.m", nil
		}
		firstRune, size := utf8.DecodeRuneInString(pkg.Name())
		className := string(unicode.ToUpper(firstRune)) + pkg.Name()[size:]
		return g.cfg.Prefix + className + ".m", nil
	}
	return "", fmt.Errorf("unknown target language: %q", lang)
}
