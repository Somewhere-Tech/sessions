//go:build !embedui

package webassets

import "io/fs"

func embeddedFS() (fs.FS, bool) {
	return nil, false
}
