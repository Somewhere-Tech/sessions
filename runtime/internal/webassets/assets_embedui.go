//go:build embedui

package webassets

import (
	"embed"
	"io/fs"
)

//go:embed dist
var bundled embed.FS

func embeddedFS() (fs.FS, bool) {
	assets, err := fs.Sub(bundled, "dist")
	return assets, err == nil
}
