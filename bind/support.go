// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package bind

import "embed"

// SupportFiles contains the runtime support files copied by gobind into
// generated binding workspaces.
//
//go:embed seq.go.support java/Seq.java java/seq_android.c.support java/seq_android.go.support java/seq_android.h objc/seq_darwin.m.support objc/seq_darwin.go.support objc/ref.h objc/seq_darwin.h
var SupportFiles embed.FS
