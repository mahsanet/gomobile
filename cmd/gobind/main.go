// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"golang.org/x/mobile/internal/gobind"
)

var (
	lang          = flag.String("lang", "", "target languages for bindings, either java, go, or objc. If empty, all languages are generated.")
	outdir        = flag.String("outdir", "", "result will be written to the directory instead of stdout.")
	javaPkg       = flag.String("javapkg", "", "custom Java package path prefix. Valid only with -lang=java.")
	soname        = flag.String("soname", "gojni", "name for the Android output shared library. Valid only with -lang=java.")
	prefix        = flag.String("prefix", "", "custom Objective-C name prefix. Valid only with -lang=objc.")
	bootclasspath = flag.String("bootclasspath", "", "Java bootstrap classpath.")
	classpath     = flag.String("classpath", "", "Java classpath.")
	tags          = flag.String("tags", "", "build tags.")
)

var usage = `The Gobind tool generates Java language bindings for Go.

For usage details, see doc.go.`

func main() {
	flag.Parse()
	var langs []string
	if *lang != "" {
		langs = strings.Split(*lang, ",")
	}
	var buildTags []string
	if *tags != "" {
		buildTags = strings.Split(*tags, ",")
	}
	if err := gobind.Run(gobind.Config{
		Langs:         langs,
		OutDir:        *outdir,
		JavaPkg:       *javaPkg,
		Soname:        *soname,
		Prefix:        *prefix,
		Bootclasspath: *bootclasspath,
		Classpath:     *classpath,
		Tags:          buildTags,
		Args:          flag.Args(),
		Stdout:        os.Stdout,
		Stderr:        os.Stderr,
	}); err != nil {
		log.Fatal(err)
	}
}

func init() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "%s\n", usage)
		flag.PrintDefaults()
	}
}
