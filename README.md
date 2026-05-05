# Go support for Mobile devices

[![Go Reference](https://pkg.go.dev/badge/golang.org/x/mobile.svg)](https://pkg.go.dev/golang.org/x/mobile)

The Go mobile repository holds packages and build tools for using Go on mobile platforms.

Package documentation as a starting point:

- [Building all-Go apps](https://golang.org/x/mobile/app)
- [Building libraries for SDK apps](https://golang.org/x/mobile/cmd/gobind)

![Caution image](doc/caution.png)

The Go Mobile project is experimental. Use this at your own risk.
While we are working hard to improve it, neither Google nor the Go
team can provide end-user support.

## Using multiple gomobile AARs in one app

By default, `gomobile bind` for Android emits an AAR containing
`jni/<abi>/libgojni.so` and Java runtime support classes in the `go`
package. If two independently built gomobile AARs are added to the same
Android app, those shared names collide.

When you control all of the Go packages, prefer building them together:

	$ gomobile bind -target=android -o all.aar example.com/auth example.com/payments

For AARs that must be built and consumed independently, pass a distinct
`-soname` for each build. The value must match `[a-zA-Z][a-zA-Z0-9_]*`.

	$ gomobile bind -target=android -soname gojni_auth -o auth.aar example.com/auth
	$ gomobile bind -target=android -soname gojni_payments -o payments.aar example.com/payments

This produces shared libraries such as `libgojni_auth.so` and namespaces
the generated JNI/runtime support symbols and classes so the AARs can be
packaged together without Android Gradle `packagingOptions` workarounds.

If `-javapkg` is set and `-soname` is omitted, gomobile derives a soname
automatically by replacing dots with underscores and prepending `gojni_`.
For example, `-javapkg com.corp.auth` derives `gojni_com_corp_auth`.

Each independently built AAR still contains its own Go runtime instance.
That avoids name collisions, but it also means each AAR has separate memory,
goroutine, and initialization overhead.

This is early work and installing the build system requires Go 1.5.
Follow the instructions on
[golang.org/wiki/Mobile](https://golang.org/wiki/Mobile)
to install the gomobile command, build the
[basic](https://golang.org/x/mobile/example/basic)
and the [bind](https://golang.org/x/mobile/example/bind) example apps.

--

Contributions to Go are appreciated. See https://go.dev/doc/contribute.

The git repository is https://go.googlesource.com/mobile.

* Bugs can be filed at the [Go issue tracker](https://go.dev/issue/new?title=x/mobile:+).
* Feature requests should preliminary be discussed on
[golang-nuts](https://groups.google.com/forum/#!forum/golang-nuts)
mailing list.
